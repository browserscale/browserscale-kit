package mail

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	noopInterval  = 15 * time.Second
	retryInterval = 5 * time.Second
)

// watcher owns the Fetcher's connection. It keeps the headers of recent
// mail in a cache and, while anyone waits, asks the server only for mail
// newer than the last UID it saw: one command per round no matter how many
// requests wait, and no request text ever goes into a command.
type watcher struct {
	f      *Fetcher
	client *Client
	active []*request
	cache  mailCache

	lastIO      time.Time
	lastAttempt time.Time
}

type mailCache struct {
	uidValidity uint32
	// lastUID is the highest UID the watcher has looked for; newer mail is
	// found by searching above it.
	lastUID uint32
	// The cache holds every mail of the mailbox when all is set, or else
	// every mail that arrived at or after from when complete is set.
	all      bool
	complete bool
	from     time.Time
	mails    map[uint32]*cachedMail
}

type cachedMail struct {
	header Header
	msg    *Message // loaded once a request wanted to look inside
}

func newWatcher(f *Fetcher) *watcher {
	return &watcher{f: f, cache: mailCache{mails: map[uint32]*cachedMail{}}}
}

func (w *watcher) run() {
	defer close(w.f.done)
	defer func() {
		if w.client != nil {
			_ = w.client.Close()
		}
	}()
	ticker := time.NewTicker(w.f.cfg.PollInterval)
	defer ticker.Stop()
	for {
		if err := w.step(); err != nil {
			w.fail(err)
		}
		select {
		case <-w.f.ctx.Done():
			return
		case req := <-w.f.requests:
			w.active = append(w.active, req)
			w.drain()
		case <-ticker.C:
		}
	}
}

func (w *watcher) drain() {
	for {
		select {
		case req := <-w.f.requests:
			w.active = append(w.active, req)
		default:
			return
		}
	}
}

func (w *watcher) step() error {
	now := time.Now()
	w.active = slices.DeleteFunc(w.active, func(r *request) bool { return r.finished(now) })
	if w.client == nil {
		if err := w.reconnect(); err != nil || w.client == nil {
			return err
		}
	}
	ctx := w.f.ctx
	if len(w.active) == 0 {
		w.prune(now)
		if now.Sub(w.lastIO) < noopInterval {
			return nil
		}
		if err := w.client.Noop(ctx); err != nil {
			return err
		}
		w.lastIO = now
		return nil
	}
	if err := w.pollNew(ctx); err != nil {
		return err
	}
	if err := w.backfill(ctx); err != nil {
		return err
	}
	if err := w.match(ctx); err != nil {
		return err
	}
	w.lastIO = time.Now()
	w.f.setErr(nil)
	w.prune(now)
	return nil
}

func (w *watcher) fail(err error) {
	if w.f.ctx.Err() != nil {
		return
	}
	w.f.cfg.Logger.Warn("mail: mailbox error, reconnecting", "err", err)
	w.f.setErr(err)
	if w.client != nil {
		_ = w.client.Close()
		w.client = nil
	}
}

// reconnect dials again, at most every retryInterval; in between it leaves
// the client nil without an error.
func (w *watcher) reconnect() error {
	if time.Since(w.lastAttempt) < retryInterval {
		return nil
	}
	w.lastAttempt = time.Now()
	ctx := w.f.ctx
	client, err := Dial(ctx, w.f.cfg)
	if err != nil {
		return err
	}
	mb, err := client.Select(ctx, w.f.cfg.Mailbox)
	if err != nil {
		_ = client.Close()
		return err
	}
	if err := w.adopt(ctx, client, mb); err != nil {
		_ = client.Close()
		return err
	}
	w.f.cfg.Logger.Info("mail: connected", "mailbox", mb.Name)
	return nil
}

// adopt takes over a freshly selected connection. The cache survives as
// long as the mailbox's UIDVALIDITY does.
func (w *watcher) adopt(ctx context.Context, client *Client, mb *Mailbox) error {
	w.client = client
	w.lastIO = time.Now()
	if mb.UIDValidity != 0 && mb.UIDValidity == w.cache.uidValidity {
		return nil
	}
	w.cache = mailCache{uidValidity: mb.UIDValidity, mails: map[uint32]*cachedMail{}}
	for _, r := range w.active {
		clear(r.checked)
	}
	if mb.UIDNext > 0 {
		w.cache.lastUID = mb.UIDNext - 1
		return nil
	}
	uids, err := client.search(ctx, Criteria{}, "")
	if err != nil {
		return err
	}
	if len(uids) > 0 {
		w.cache.lastUID = uids[len(uids)-1]
	}
	return nil
}

// pollNew loads the headers of mail that arrived since the last round.
func (w *watcher) pollNew(ctx context.Context) error {
	uids, err := w.client.search(ctx, Criteria{}, strconv.FormatUint(uint64(w.cache.lastUID)+1, 10)+":*")
	if err != nil {
		return err
	}
	// n:* always includes the newest message, even when its UID is below n.
	uids = slices.DeleteFunc(uids, func(uid uint32) bool { return uid <= w.cache.lastUID })
	if len(uids) == 0 {
		return nil
	}
	if err := w.load(ctx, uids); err != nil {
		return err
	}
	w.cache.lastUID = uids[len(uids)-1]
	return nil
}

// backfill loads older mail the waiting requests reach back to. SINCE only
// compares dates, in the server's time zone, so it searches from a day
// earlier than needed; the exact bound is applied to INTERNALDATE.
func (w *watcher) backfill(ctx context.Context) error {
	c := &w.cache
	if c.all {
		return nil
	}
	var oldest time.Time
	all := false
	for _, r := range w.active {
		if r.since.IsZero() {
			all = true
			break
		}
		if oldest.IsZero() || r.since.Before(oldest) {
			oldest = r.since
		}
	}
	if !all && c.complete && !oldest.Before(c.from) {
		return nil
	}
	var day time.Time
	q := Criteria{}
	if !all {
		u := oldest.UTC()
		day = time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
		q.Since = day.AddDate(0, 0, -1)
	}
	if c.lastUID > 0 {
		uids, err := w.client.search(ctx, q, "1:"+strconv.FormatUint(uint64(c.lastUID), 10))
		if err != nil {
			return err
		}
		uids = slices.DeleteFunc(uids, func(uid uint32) bool { return c.mails[uid] != nil })
		if err := w.load(ctx, uids); err != nil {
			return err
		}
	}
	if all {
		c.all = true
	} else {
		c.complete, c.from = true, day
	}
	return nil
}

func (w *watcher) load(ctx context.Context, uids []uint32) error {
	if len(uids) == 0 {
		return nil
	}
	headers, err := w.client.Headers(ctx, uids)
	if err != nil {
		return err
	}
	for _, h := range headers {
		w.cache.mails[h.UID] = &cachedMail{header: h}
	}
	return nil
}

// match offers the cached mail to every waiting request, newest first, and
// loads a mail's content only for requests whose sender, recipient and
// subject fit.
func (w *watcher) match(ctx context.Context) error {
	log := w.f.cfg.Logger
	for _, r := range w.active {
		if r.finished(time.Now()) {
			continue
		}
		for _, m := range w.candidates(r) {
			uid := m.header.UID
			if m.msg == nil {
				msg, err := w.client.Fetch(ctx, uid)
				if errors.Is(err, ErrNotFound) {
					delete(w.cache.mails, uid)
					continue
				}
				if err != nil {
					return err
				}
				m.msg = msg
			}
			code, ok := r.extract(m.msg)
			if !ok {
				r.checked[uid] = true
				log.Debug("mail: mail matched the query but not its Code pattern",
					"uid", uid, "subject", m.header.Subject, "body", m.msg.Body)
				continue
			}
			r.deliver(outcome{res: &Result{Code: code, Message: m.msg}})
			if r.query.Delete {
				if err := w.client.Delete(ctx, uid); err != nil {
					var ce *CommandError
					if !errors.As(err, &ce) {
						return err
					}
					log.Warn("mail: deleting the matched mail failed", "uid", uid, "err", err)
				} else {
					delete(w.cache.mails, uid)
				}
			}
			break
		}
	}
	return nil
}

func (w *watcher) candidates(r *request) []*cachedMail {
	var out []*cachedMail
	for uid, m := range w.cache.mails {
		h := &m.header
		if r.checked[uid] || (!r.since.IsZero() && h.InternalDate.Before(r.since)) {
			continue
		}
		if !anyAddress(h.From, r.query.From) || !anyAddress(h.To, r.query.To) ||
			!containsFold(h.Subject, r.query.Subject) {
			continue
		}
		out = append(out, m)
	}
	slices.SortFunc(out, func(a, b *cachedMail) int {
		if c := b.header.InternalDate.Compare(a.header.InternalDate); c != 0 {
			return c
		}
		return cmp.Compare(b.header.UID, a.header.UID)
	})
	return out
}

// prune forgets mail no waiting request can reach anymore, and the content
// of mail while nobody waits.
func (w *watcher) prune(now time.Time) {
	c := &w.cache
	cutoff := now.Add(-w.f.cfg.Window - 48*time.Hour)
	for _, r := range w.active {
		if r.since.IsZero() {
			return
		}
		if r.since.Before(cutoff) {
			cutoff = r.since
		}
	}
	cutoff = cutoff.AddDate(0, 0, -1)
	for uid, m := range c.mails {
		if len(w.active) == 0 {
			m.msg = nil
		}
		if m.header.InternalDate.Before(cutoff) {
			delete(c.mails, uid)
		}
	}
	if c.all {
		c.all, c.complete, c.from = false, true, cutoff
	} else if c.complete && c.from.Before(cutoff) {
		c.from = cutoff
	}
}

func anyAddress(list []Address, needle string) bool {
	if needle == "" {
		return true
	}
	for _, a := range list {
		if containsFold(a.String(), needle) {
			return true
		}
	}
	return false
}

func containsFold(s, sub string) bool {
	return sub == "" || strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}
