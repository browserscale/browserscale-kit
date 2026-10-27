package mail

import (
	"context"
	"fmt"
	"regexp"
	"sync"
	"time"
)

// Fetcher watches one mailbox over a single kept-alive connection and hands
// every waiting worker the mail meant for it. Create one per mailbox at
// start and share it; hundreds of parallel waits cost one poll per round.
type Fetcher struct {
	cfg      Config
	requests chan *request
	ctx      context.Context
	cancel   context.CancelFunc
	done     chan struct{}

	mu      sync.Mutex
	lastErr error
}

// Query describes the mail a worker waits for. Strings match as
// case-insensitive substrings, empty ones match anything.
type Query struct {
	From    string // sender, e.g. noreply@site.com; matches name or address
	To      string // recipient, e.g. this account's address on a catch-all
	Subject string
	// Code is a regular expression for what to pull out of the body; its
	// first group if it has one, else the whole match. Empty accepts the
	// first matching mail and leaves Result.Code empty.
	Code string
	// Since is the earliest arrival that counts. Config.ClockSkew is taken
	// off. When zero, Expect uses the moment it was called and FetchCode the
	// start of Config.Window.
	Since time.Time
	// AllMail drops the time bound and considers the whole mailbox.
	AllMail bool
	// Timeout bounds the wait, 5 minutes when zero; a shorter ctx wins.
	Timeout time.Duration
	// Delete removes the matched mail, so a reused mailbox can't match it again.
	Delete bool
}

// Result is the mail a Wait found.
type Result struct {
	Code    string
	Message *Message
}

// NewFetcher connects and logs in right away, so a wrong host or password
// fails here, then keeps watching until Close.
func NewFetcher(ctx context.Context, cfg Config) (*Fetcher, error) {
	cfg = cfg.withDefaults()
	client, err := Dial(ctx, cfg)
	if err != nil {
		return nil, err
	}
	mb, err := client.Select(ctx, cfg.Mailbox)
	if err != nil {
		_ = client.Close()
		return nil, err
	}
	runCtx, cancel := context.WithCancel(context.Background())
	f := &Fetcher{
		cfg:      cfg,
		requests: make(chan *request, 1024),
		ctx:      runCtx,
		cancel:   cancel,
		done:     make(chan struct{}),
	}
	w := newWatcher(f)
	if err := w.adopt(ctx, client, mb); err != nil {
		_ = client.Close()
		cancel()
		return nil, err
	}
	go w.run()
	return f, nil
}

// Expect starts watching for a mail right away. Call it before the action
// that makes the site send the mail, then collect the result with Code or
// Result afterwards: only mail arriving from now on counts (less
// Config.ClockSkew), so a stale code on a reused mailbox can't match.
//
//	wait := f.Expect(mail.Query{From: "noreply@site.com", To: email, Code: `(\d{6})`})
//	page.Click(ctx, browserscale.CSS("#send-code"))
//	code, err := wait.Code(ctx)
func (f *Fetcher) Expect(q Query) *Wait {
	now := time.Now()
	return f.submit(q, now, now)
}

// FetchCode waits for a mail and returns its code, for when the mail may
// already be there. Without Since it considers the last Config.Window.
func (f *Fetcher) FetchCode(ctx context.Context, q Query) (string, error) {
	now := time.Now()
	return f.submit(q, now, now.Add(-f.cfg.Window)).Code(ctx)
}

// Close stops watching, fails the waits still open with ErrClosed and logs
// out.
func (f *Fetcher) Close() error {
	f.cancel()
	<-f.done
	return nil
}

func (f *Fetcher) submit(q Query, now, defaultSince time.Time) *Wait {
	w := &Wait{f: f}
	var re *regexp.Regexp
	if q.Code != "" {
		var err error
		if re, err = regexp.Compile(q.Code); err != nil {
			w.err = fmt.Errorf("mail: invalid Code pattern: %w", err)
			return w
		}
	}
	since := q.Since
	if since.IsZero() {
		since = defaultSince
	}
	since = since.Add(-f.cfg.ClockSkew)
	if q.AllMail {
		since = time.Time{}
	}
	timeout := q.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	req := &request{
		query:    q,
		code:     re,
		since:    since,
		deadline: now.Add(timeout),
		result:   make(chan outcome, 1),
		gone:     make(chan struct{}),
		checked:  map[uint32]bool{},
	}
	select {
	case f.requests <- req:
		w.req = req
	case <-f.done:
		w.err = ErrClosed
	}
	return w
}

func (f *Fetcher) setErr(err error) {
	f.mu.Lock()
	f.lastErr = err
	f.mu.Unlock()
}

func (f *Fetcher) timeoutErr() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.lastErr != nil {
		return fmt.Errorf("%w: no matching mail arrived (mailbox unreachable: %v)", ErrTimeout, f.lastErr)
	}
	return fmt.Errorf("%w: no matching mail arrived", ErrTimeout)
}

// Wait is a pending Expect.
type Wait struct {
	f   *Fetcher
	req *request
	err error

	once sync.Once
	res  *Result
	rerr error
}

// Code waits for the mail and returns its code.
func (w *Wait) Code(ctx context.Context) (string, error) {
	res, err := w.Result(ctx)
	if err != nil {
		return "", err
	}
	return res.Code, nil
}

// Result waits for the mail and returns it with its code. It waits only
// once; later calls return the same answer.
func (w *Wait) Result(ctx context.Context) (*Result, error) {
	w.once.Do(func() { w.res, w.rerr = w.wait(ctx) })
	return w.res, w.rerr
}

// Cancel gives up the wait.
func (w *Wait) Cancel() {
	if w.req != nil {
		w.req.leave()
	}
}

func (w *Wait) wait(ctx context.Context) (*Result, error) {
	if w.err != nil {
		return nil, w.err
	}
	defer w.req.leave()
	timer := time.NewTimer(time.Until(w.req.deadline))
	defer timer.Stop()
	select {
	case o := <-w.req.result:
		return o.res, o.err
	case <-timer.C:
		select {
		case o := <-w.req.result:
			return o.res, o.err
		default:
		}
		return nil, w.f.timeoutErr()
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-w.f.done:
		return nil, ErrClosed
	}
}

type outcome struct {
	res *Result
	err error
}

type request struct {
	query    Query
	code     *regexp.Regexp
	since    time.Time // zero: no lower bound
	deadline time.Time
	result   chan outcome
	gone     chan struct{}
	goneOnce sync.Once
	checked  map[uint32]bool // mails this request looked at and rejected
}

func (r *request) leave() { r.goneOnce.Do(func() { close(r.gone) }) }

func (r *request) finished(now time.Time) bool {
	select {
	case <-r.gone:
		return true
	default:
		return !now.Before(r.deadline)
	}
}

func (r *request) deliver(o outcome) {
	select {
	case r.result <- o:
	default:
	}
	r.leave()
}

// extract applies the Code pattern to a message.
func (r *request) extract(m *Message) (string, bool) {
	if r.code == nil {
		return "", true
	}
	sub := r.code.FindStringSubmatch(m.Body)
	switch {
	case sub == nil:
		return "", false
	case len(sub) > 1:
		return sub[1], true
	default:
		return sub[0], true
	}
}
