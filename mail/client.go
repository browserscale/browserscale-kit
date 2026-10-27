package mail

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Client is one logged-in IMAP connection. It is safe for concurrent use;
// commands run one after another. For waiting on codes use a Fetcher, which
// keeps a Client alive and reconnects it.
type Client struct {
	mu      sync.Mutex
	conn    *conn
	mailbox *Mailbox
}

// Mailbox is the state of the selected mailbox.
type Mailbox struct {
	Name           string
	Exists         uint32 // number of messages
	UIDValidity    uint32 // UIDs stay valid as long as this doesn't change
	UIDNext        uint32 // the UID the next message will get, 0 if unknown
	ReadOnly       bool
	PermanentFlags []string
}

// Criteria select messages for Search. Strings match as case-insensitive
// substrings of the header or body, as the server's SEARCH does. The zero
// value matches every message.
type Criteria struct {
	From    string
	To      string
	Subject string
	Body    string
	// Since and Before compare dates only, without time of day, in the
	// server's view of the INTERNALDATE.
	Since  time.Time
	Before time.Time
	// Limit keeps only the newest Limit UIDs, 0 keeps all.
	Limit int
}

// Dial connects over TLS and logs in.
func Dial(ctx context.Context, cfg Config) (*Client, error) {
	cfg = cfg.withDefaults()
	dialCtx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	nc, err := cfg.connect(dialCtx)
	cancel()
	if err != nil {
		return nil, err
	}
	c := &Client{conn: newConn(nc, cfg)}
	if err := c.conn.open(ctx, cfg.Username, cfg.Password); err != nil {
		_ = nc.Close()
		return nil, err
	}
	return c, nil
}

func (c *Client) exec(ctx context.Context, cmd *command) ([]*response, *response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn.execute(ctx, cmd)
}

// Capabilities lists what the server announced, upper-cased.
func (c *Client) Capabilities() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.conn.caps))
	for name := range c.conn.caps {
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

// Has reports whether the server announced a capability, e.g. UIDPLUS.
func (c *Client) Has(capability string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn.caps[strings.ToUpper(capability)]
}

// Mailbox is the selected mailbox, nil before Select.
func (c *Client) Mailbox() *Mailbox {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.mailbox
}

// List returns the names of all mailboxes.
func (c *Client) List(ctx context.Context) ([]string, error) {
	untagged, _, err := c.exec(ctx, newCommand("LIST").str("").str("*"))
	if err != nil {
		return nil, err
	}
	var names []string
	for _, r := range untagged {
		if r.kind != "LIST" {
			continue
		}
		if _, err := r.data.value(); err != nil { // flags
			continue
		}
		if _, err := r.data.value(); err != nil { // hierarchy delimiter
			continue
		}
		if name, err := r.data.value(); err == nil {
			names = append(names, name.nstring())
		}
	}
	return names, nil
}

// Select opens a mailbox for reading and deleting.
func (c *Client) Select(ctx context.Context, name string) (*Mailbox, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.mailbox = nil
	untagged, done, err := c.conn.execute(ctx, newCommand("SELECT").str(name))
	if err != nil {
		return nil, err
	}
	mb := &Mailbox{Name: name}
	for _, r := range append(untagged, done) {
		if r.kind == "EXISTS" {
			mb.Exists = r.number
		}
		key, arg, _ := strings.Cut(r.code, " ")
		switch strings.ToUpper(key) {
		case "UIDVALIDITY":
			n, _ := strconv.ParseUint(arg, 10, 32)
			mb.UIDValidity = uint32(n)
		case "UIDNEXT":
			n, _ := strconv.ParseUint(arg, 10, 32)
			mb.UIDNext = uint32(n)
		case "PERMANENTFLAGS":
			mb.PermanentFlags = strings.Fields(strings.Trim(arg, "()"))
		case "READ-ONLY":
			mb.ReadOnly = true
		case "READ-WRITE":
			mb.ReadOnly = false
		}
	}
	c.mailbox = mb
	return mb, nil
}

// Noop asks the server for news and keeps the connection alive.
func (c *Client) Noop(ctx context.Context) error {
	_, _, err := c.exec(ctx, newCommand("NOOP"))
	return err
}

// Search returns the UIDs of the messages in the selected mailbox that
// match, in ascending order.
func (c *Client) Search(ctx context.Context, q Criteria) ([]uint32, error) {
	uids, err := c.search(ctx, q, "")
	if err != nil {
		return nil, err
	}
	if q.Limit > 0 && len(uids) > q.Limit {
		uids = uids[len(uids)-q.Limit:]
	}
	return uids, nil
}

// search runs UID SEARCH, optionally limited to a UID range.
func (c *Client) search(ctx context.Context, q Criteria, uidRange string) ([]uint32, error) {
	cmd := newCommand("UID SEARCH")
	if !isASCII(q.From + q.To + q.Subject + q.Body) {
		cmd.atom("CHARSET UTF-8")
	}
	n := 0
	for _, kv := range [][2]string{{"FROM", q.From}, {"TO", q.To}, {"SUBJECT", q.Subject}, {"BODY", q.Body}} {
		if kv[1] != "" {
			cmd.atom(kv[0]).str(kv[1])
			n++
		}
	}
	if !q.Since.IsZero() {
		cmd.atom("SINCE " + imapDate(q.Since))
		n++
	}
	if !q.Before.IsZero() {
		cmd.atom("BEFORE " + imapDate(q.Before))
		n++
	}
	if uidRange != "" {
		cmd.atom("UID " + uidRange)
		n++
	}
	if n == 0 {
		cmd.atom("ALL")
	}
	untagged, _, err := c.exec(ctx, cmd)
	if err != nil {
		return nil, err
	}
	var uids []uint32
	for _, r := range untagged {
		if r.kind != "SEARCH" {
			continue
		}
		for !r.data.atEnd() {
			v, err := r.data.value()
			if err != nil {
				break
			}
			if uid, ok := v.number(); ok && uid > 0 {
				uids = append(uids, uid)
			}
		}
	}
	slices.Sort(uids)
	return slices.Compact(uids), nil
}

// Headers returns envelope and INTERNALDATE of the given UIDs, in ascending
// UID order. UIDs the mailbox doesn't hold are left out.
func (c *Client) Headers(ctx context.Context, uids []uint32) ([]Header, error) {
	want := make(map[uint32]bool, len(uids))
	for _, uid := range uids {
		want[uid] = true
	}
	var out []Header
	for _, set := range uidSets(uids) {
		untagged, _, err := c.exec(ctx, newCommand("UID FETCH").atom(set).atom("(UID INTERNALDATE ENVELOPE)"))
		if err != nil {
			return nil, err
		}
		for _, r := range untagged {
			items := fetchItems(r)
			if items == nil {
				continue
			}
			if h, ok := headerFromItems(items); ok && want[h.UID] {
				want[h.UID] = false
				out = append(out, h)
			}
		}
	}
	slices.SortFunc(out, func(a, b Header) int { return cmp.Compare(a.UID, b.UID) })
	return out, nil
}

// Fetch returns a complete message. It doesn't mark the message as seen.
func (c *Client) Fetch(ctx context.Context, uid uint32) (*Message, error) {
	set := strconv.FormatUint(uint64(uid), 10)
	untagged, _, err := c.exec(ctx, newCommand("UID FETCH").atom(set).atom("(UID INTERNALDATE ENVELOPE BODY.PEEK[])"))
	if err != nil {
		return nil, err
	}
	for _, r := range untagged {
		items := fetchItems(r)
		if items == nil {
			continue
		}
		h, ok := headerFromItems(items)
		if !ok || h.UID != uid {
			continue
		}
		raw := []byte(items["BODY[]"].text)
		m := &Message{Header: h, Raw: raw}
		m.Text, m.HTML, m.Body = readBody(raw)
		return m, nil
	}
	return nil, fmt.Errorf("%w: UID %d", ErrNotFound, uid)
}

// Delete removes messages from the selected mailbox for good. Servers
// without UIDPLUS can only expunge every message marked \Deleted at once,
// which then includes messages other clients marked.
func (c *Client) Delete(ctx context.Context, uids ...uint32) error {
	mb := c.Mailbox()
	if mb == nil {
		return errors.New("mail: Delete needs a selected mailbox")
	}
	if mb.ReadOnly {
		return fmt.Errorf("mail: mailbox %s is read-only", mb.Name)
	}
	for _, set := range uidSets(uids) {
		if _, _, err := c.exec(ctx, newCommand("UID STORE").atom(set).atom(`+FLAGS.SILENT (\Deleted)`)); err != nil {
			var ce *CommandError
			if !errors.As(err, &ce) {
				return err
			}
			// Some servers reject UID STORE; go by message numbers instead.
			if err := c.storeBySequence(ctx, set); err != nil {
				return err
			}
		}
		if c.Has("UIDPLUS") {
			_, _, err := c.exec(ctx, newCommand("UID EXPUNGE").atom(set))
			if err == nil {
				continue
			}
			var ce *CommandError
			if !errors.As(err, &ce) {
				return err
			}
		}
		if _, _, err := c.exec(ctx, newCommand("EXPUNGE")); err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) storeBySequence(ctx context.Context, set string) error {
	untagged, _, err := c.exec(ctx, newCommand("UID FETCH").atom(set).atom("(UID)"))
	if err != nil {
		return err
	}
	var seqs []uint32
	for _, r := range untagged {
		if r.kind == "FETCH" && r.number > 0 {
			seqs = append(seqs, r.number)
		}
	}
	if len(seqs) == 0 {
		return fmt.Errorf("%w: UID %s", ErrNotFound, set)
	}
	for _, seqSet := range uidSets(seqs) {
		if _, _, err := c.exec(ctx, newCommand("STORE").atom(seqSet).atom(`+FLAGS.SILENT (\Deleted)`)); err != nil {
			return err
		}
	}
	return nil
}

// Close logs out and closes the connection.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn.logout()
}

// fetchItems reads the item list of a FETCH response into a map keyed by
// upper-cased item name, whatever order the server sends them in.
func fetchItems(r *response) map[string]value {
	if r.kind != "FETCH" {
		return nil
	}
	sc := r.data
	sc.skipSpaces()
	if sc.i >= len(sc.s) || sc.s[sc.i] != '(' {
		return nil
	}
	sc.i++
	items := map[string]value{}
	for {
		sc.skipSpaces()
		if sc.i >= len(sc.s) {
			return nil
		}
		if sc.s[sc.i] == ')' {
			return items
		}
		name := strings.ToUpper(sc.itemName())
		if name == "" {
			return nil
		}
		v, err := sc.value()
		if err != nil {
			return nil
		}
		items[name] = v
	}
}
