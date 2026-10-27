package mail

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	netmail "net/mail"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeServer is a small IMAP server that behaves like the servers the client
// meets: configurable item order, date padding, line limits, time zone and
// capabilities.
type fakeServer struct {
	t *testing.T

	mu          sync.Mutex
	mails       []*fakeMail
	nextUID     uint32
	uidValidity uint32
	caps        string
	username    string
	password    string
	tz          *time.Location
	uidLast     bool   // send UID as the last FETCH item, as Maddy does
	padDay      bool   // INTERNALDATE " 7-Oct-2026" instead of "07-Oct-2026"
	maxLine     int    // refuse longer command lines with BAD
	hangOn      string // never answer this command
	failSelect  bool
	refuseLogin bool
	noUIDNext   bool
	commands    []string
	conns       []net.Conn
}

type fakeMail struct {
	uid      uint32
	internal time.Time
	raw      string
	deleted  bool
}

func newFakeServer(t *testing.T) *fakeServer {
	s := &fakeServer{
		t: t, nextUID: 1, uidValidity: 7, caps: "IMAP4rev1 UIDPLUS",
		username: "user@example.com", password: "secret", tz: time.UTC,
	}
	t.Cleanup(s.dropConnections)
	return s
}

func (s *fakeServer) config() Config {
	return Config{
		Username: s.username, Password: s.password,
		Timeout: 2 * time.Second, PollInterval: 20 * time.Millisecond,
		dial: s.dial,
	}
}

func (s *fakeServer) dial(context.Context) (net.Conn, error) {
	client, server := net.Pipe()
	s.mu.Lock()
	s.conns = append(s.conns, server)
	s.mu.Unlock()
	go s.serve(server)
	return client, nil
}

func (s *fakeServer) dropConnections() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.conns {
		_ = c.Close()
	}
	s.conns = nil
}

func (s *fakeServer) add(internal time.Time, raw string) uint32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	uid := s.nextUID
	s.nextUID++
	s.mails = append(s.mails, &fakeMail{uid: uid, internal: internal, raw: raw})
	return uid
}

func (s *fakeServer) count(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.commands {
		if c == name {
			n++
		}
	}
	return n
}

func (s *fakeServer) has(uid uint32) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.mails {
		if m.uid == uid {
			return true
		}
	}
	return false
}

func mailText(from, to, subject, body string) string {
	return "From: " + from + "\r\nTo: " + to + "\r\nSubject: " + subject +
		"\r\nDate: Mon, 26 Oct 2026 20:30:02 +0000\r\nMessage-ID: <id@test>\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n\r\n" + body + "\r\n"
}

// readCommand reads one command line with its literals, asking for
// synchronizing literals with a continuation.
func readCommand(r *bufio.Reader, w *bufio.Writer) (string, error) {
	var b strings.Builder
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return "", err
		}
		b.WriteString(line)
		trimmed := strings.TrimRight(line, "\r\n")
		n, ok := trailingLiteral(trimmed)
		if !ok {
			return b.String(), nil
		}
		if !strings.HasSuffix(trimmed, "+}") {
			w.WriteString("+ go ahead\r\n")
			w.Flush()
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(r, buf); err != nil {
			return "", err
		}
		b.Write(buf)
	}
}

func (s *fakeServer) serve(conn net.Conn) {
	defer conn.Close()
	r, w := bufio.NewReader(conn), bufio.NewWriter(conn)
	reply := func(lines ...string) {
		for _, l := range lines {
			w.WriteString(l + "\r\n")
		}
		w.Flush()
	}
	s.mu.Lock()
	reply("* OK [CAPABILITY " + s.caps + "] fake ready")
	s.mu.Unlock()
	readOnly := false
	for {
		line, err := readCommand(r, w)
		if err != nil {
			return
		}
		sc := &scanner{s: line}
		tag := sc.word()
		name := strings.ToUpper(sc.word())
		if name == "UID" {
			name += " " + strings.ToUpper(sc.word())
		}
		s.mu.Lock()
		s.commands = append(s.commands, name)
		hang := name == s.hangOn
		tooLong := s.maxLine > 0 && len(strings.SplitN(line, "\r\n", 2)[0]) > s.maxLine
		s.mu.Unlock()
		if hang {
			_, _ = io.Copy(io.Discard, r)
			return
		}
		if tooLong {
			reply(tag + " BAD line too long")
			continue
		}
		s.mu.Lock()
		out, status := s.handle(name, sc, &readOnly)
		s.mu.Unlock()
		reply(append(out, tag+" "+status)...)
		if name == "LOGOUT" {
			return
		}
	}
}

func (s *fakeServer) handle(name string, sc *scanner, readOnly *bool) ([]string, string) {
	str := func() string {
		v, err := sc.value()
		if err != nil {
			s.t.Errorf("%s: bad argument: %v", name, err)
		}
		return v.text
	}
	switch name {
	case "CAPABILITY":
		return []string{"* CAPABILITY " + s.caps}, "OK done"
	case "LOGIN":
		u, p := str(), str()
		if s.refuseLogin || u != s.username || p != s.password {
			return nil, "NO [AUTHENTICATIONFAILED] wrong credentials"
		}
		return nil, "OK [CAPABILITY " + s.caps + " MOVE] logged in"
	case "SELECT":
		if s.failSelect {
			return nil, "NO mailbox unavailable"
		}
		out := []string{
			fmt.Sprintf("* %d EXISTS", len(s.live())),
			fmt.Sprintf("* OK [UIDVALIDITY %d] ok", s.uidValidity),
			`* OK [PERMANENTFLAGS (\Deleted \Seen \*)] ok`,
		}
		if !s.noUIDNext {
			out = append(out, fmt.Sprintf("* OK [UIDNEXT %d] ok", s.nextUID))
		}
		*readOnly = false
		return out, "OK [READ-WRITE] selected"
	case "LIST":
		return []string{`* LIST (\HasNoChildren) "/" INBOX`, `* LIST (\HasNoChildren) "/" "Junk Mail"`}, "OK done"
	case "NOOP":
		return nil, "OK done"
	case "LOGOUT":
		return []string{"* BYE bye"}, "OK done"
	case "UID SEARCH":
		return s.search(sc)
	case "UID FETCH":
		set := sc.word()
		items, _ := sc.value()
		var out []string
		for i, m := range s.live() {
			if inSet(set, m.uid, s.maxUID()) {
				out = append(out, s.fetchLine(i+1, m, items))
			}
		}
		return out, "OK done"
	case "UID STORE", "STORE":
		set := sc.word()
		for i, m := range s.live() {
			id, max := m.uid, s.maxUID()
			if name == "STORE" {
				id, max = uint32(i+1), uint32(len(s.mails))
			}
			if inSet(set, id, max) {
				m.deleted = true
			}
		}
		return nil, "OK stored"
	case "UID EXPUNGE":
		if !strings.Contains(s.caps, "UIDPLUS") {
			return nil, "BAD unknown command"
		}
		set := sc.word()
		return s.expunge(func(m *fakeMail) bool { return inSet(set, m.uid, s.maxUID()) }), "OK expunged"
	case "EXPUNGE":
		return s.expunge(func(*fakeMail) bool { return true }), "OK expunged"
	}
	return nil, "BAD unknown command " + name
}

func (s *fakeServer) live() []*fakeMail { return s.mails }

func (s *fakeServer) maxUID() uint32 {
	if len(s.mails) == 0 {
		return 0
	}
	return s.mails[len(s.mails)-1].uid
}

func (s *fakeServer) expunge(which func(*fakeMail) bool) []string {
	var out []string
	kept := s.mails[:0]
	for i, m := range s.mails {
		if m.deleted && which(m) {
			out = append(out, fmt.Sprintf("* %d EXPUNGE", i+1-len(out)))
			continue
		}
		kept = append(kept, m)
	}
	s.mails = kept
	return out
}

func (s *fakeServer) search(sc *scanner) ([]string, string) {
	conds := []func(*fakeMail) bool{}
	utf8 := false
	for !sc.atEnd() {
		key := strings.ToUpper(sc.word())
		switch key {
		case "ALL":
		case "CHARSET":
			utf8 = strings.EqualFold(sc.word(), "UTF-8")
		case "FROM", "TO", "SUBJECT", "BODY":
			v, err := sc.value()
			if err != nil {
				return nil, "BAD bad search string"
			}
			if !isASCII(v.text) && !utf8 {
				return nil, "BAD 8-bit text without CHARSET"
			}
			needle := strings.ToLower(v.text)
			field := key
			conds = append(conds, func(m *fakeMail) bool {
				msg, err := netmail.ReadMessage(strings.NewReader(m.raw))
				if err != nil {
					return false
				}
				hay := msg.Header.Get(field)
				if field == "BODY" {
					b, _ := io.ReadAll(msg.Body)
					hay = string(b)
				}
				return strings.Contains(strings.ToLower(hay), needle)
			})
		case "SINCE", "BEFORE":
			day, err := time.ParseInLocation("2-Jan-2006", sc.word(), s.tz)
			if err != nil {
				return nil, "BAD bad date"
			}
			since := key == "SINCE"
			conds = append(conds, func(m *fakeMail) bool {
				l := m.internal.In(s.tz)
				d := time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, s.tz)
				if since {
					return !d.Before(day)
				}
				return d.Before(day)
			})
		case "UID":
			set := sc.word()
			max := s.maxUID()
			conds = append(conds, func(m *fakeMail) bool { return inSet(set, m.uid, max) })
		default:
			return nil, "BAD unknown search key " + key
		}
	}
	var hits []string
	for _, m := range s.live() {
		if !slices.ContainsFunc(conds, func(c func(*fakeMail) bool) bool { return !c(m) }) {
			hits = append(hits, strconv.FormatUint(uint64(m.uid), 10))
		}
	}
	return []string{strings.TrimSpace("* SEARCH " + strings.Join(hits, " "))}, "OK done"
}

// inSet reports whether n is in a sequence set like 1:4,7,9:*. As in IMAP,
// n:* also covers max when max < n.
func inSet(set string, n, max uint32) bool {
	num := func(s string) uint32 {
		if s == "*" {
			return max
		}
		v, _ := strconv.ParseUint(s, 10, 32)
		return uint32(v)
	}
	for _, part := range strings.Split(set, ",") {
		lo, hi, ok := strings.Cut(part, ":")
		a, b := num(lo), num(lo)
		if ok {
			b = num(hi)
		}
		if a > b {
			a, b = b, a
		}
		if n >= a && n <= b {
			return true
		}
	}
	return false
}

func (s *fakeServer) fetchLine(seq int, m *fakeMail, items value) string {
	var parts []string
	for _, it := range items.list {
		switch strings.ToUpper(it.text) {
		case "INTERNALDATE":
			layout := "02-Jan-2006 15:04:05 -0700"
			if s.padDay {
				layout = "_2-Jan-2006 15:04:05 -0700"
			}
			parts = append(parts, `INTERNALDATE "`+m.internal.In(s.tz).Format(layout)+`"`)
		case "ENVELOPE":
			parts = append(parts, "ENVELOPE "+envelopeOf(m.raw))
		case "BODY.PEEK[]":
			parts = append(parts, fmt.Sprintf("BODY[] {%d}\r\n%s", len(m.raw), m.raw))
		case "BODY[]":
			s.t.Errorf("client fetched BODY[], which marks the mail as seen")
		}
	}
	// UID FETCH answers carry the UID whether asked for or not.
	uid := fmt.Sprintf("UID %d", m.uid)
	if s.uidLast {
		parts = append(parts, uid)
	} else {
		parts = append([]string{uid}, parts...)
	}
	return fmt.Sprintf("* %d FETCH (%s)", seq, strings.Join(parts, " "))
}

func nstringOf(s string, present bool) string {
	if !present {
		return "NIL"
	}
	if quotable(s) {
		return quote(s)
	}
	return fmt.Sprintf("{%d}\r\n%s", len(s), s)
}

func envelopeOf(raw string) string {
	msg, err := netmail.ReadMessage(strings.NewReader(raw))
	if err != nil {
		return "NIL"
	}
	h := msg.Header
	field := func(name string) string {
		v, ok := h[name]
		if !ok {
			return "NIL"
		}
		return nstringOf(v[0], true)
	}
	addrs := func(name string) string {
		v := h.Get(name)
		if v == "" {
			return "NIL"
		}
		list, err := netmail.ParseAddressList(v)
		if err != nil {
			return "NIL"
		}
		var b strings.Builder
		b.WriteString("(")
		for _, a := range list {
			mailbox, host, _ := strings.Cut(a.Address, "@")
			fmt.Fprintf(&b, "(%s NIL %s %s)", nstringOf(a.Name, a.Name != ""), quote(mailbox), quote(host))
		}
		b.WriteString(")")
		return b.String()
	}
	from := addrs("From")
	return "(" + strings.Join([]string{
		field("Date"), field("Subject"), from, from, from,
		addrs("To"), addrs("Cc"), addrs("Bcc"), field("In-Reply-To"), field("Message-Id"),
	}, " ") + ")"
}
