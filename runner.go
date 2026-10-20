package kit

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	browserscale "github.com/browserscale/browserscale-go"
	"github.com/browserscale/browserscale-kit/proxy"
	"github.com/browserscale/browserscale-kit/store"
)

// hooks are the calls into browserscale, swapped in tests.
type hooks struct {
	rent func(ctx context.Context, c *browserscale.BrowserConfig) (*browserscale.CloudBrowser, error)
	// proxyOf is the proxy a browser goes through, as the session reports it.
	proxyOf func(ctx context.Context, b *browserscale.CloudBrowser) (proxy.Proxy, error)
	attach  func(ctx context.Context, grpc, key, session string) (*browserscale.CloudBrowser, error)
	release func(b *browserscale.CloudBrowser, keep bool)
	session func(b *browserscale.CloudBrowser) (id, grpc string)
	newPage func(ctx context.Context, b *browserscale.CloudBrowser, attached bool) (*browserscale.Page, error)
	shot    func(ctx context.Context, p *browserscale.Page) ([]byte, error)
	sleep   func(ctx context.Context, d time.Duration) error
}

func defaultHooks() hooks {
	return hooks{
		rent: browserscale.RentBrowser,
		proxyOf: func(ctx context.Context, b *browserscale.CloudBrowser) (proxy.Proxy, error) {
			id, err := b.GetClientIdentity(ctx)
			if err != nil {
				return proxy.Proxy{}, err
			}
			p := id.Proxy
			return proxy.Proxy{Scheme: p.Scheme, Host: p.Host, Port: p.Port, Username: p.Username, Password: p.Password}, nil
		},
		attach: browserscale.ConnectSession,
		release: func(b *browserscale.CloudBrowser, keep bool) {
			if keep {
				_ = b.CloseConn()
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			_, _ = b.StopBrowser(ctx)
		},
		session: func(b *browserscale.CloudBrowser) (string, string) { return b.SessionId(), b.GrpcUrl() },
		newPage: func(ctx context.Context, b *browserscale.CloudBrowser, attached bool) (*browserscale.Page, error) {
			if attached {
				return b.FocusedPage(ctx)
			}
			return b.CreatePage(ctx)
		},
		shot: func(ctx context.Context, p *browserscale.Page) ([]byte, error) {
			res, err := p.Screenshot(ctx, "jpeg", 70)
			if err != nil {
				return nil, err
			}
			return base64.StdEncoding.DecodeString(res.DataBase64)
		},
		sleep: func(ctx context.Context, d time.Duration) error {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-t.C:
				return nil
			}
		},
	}
}

type runner struct {
	name    string
	core    *runCore
	ev      *events
	saves   *saver
	opts    Options
	h       hooks
	dataDir string
	runDir  string
	key     string
	proxies *proxies
	// country is the managed proxies' country, when the run uses them.
	country   string
	configure func(*browserscale.BrowserConfig)
	lease     time.Duration
	retries   int
	stopAfter int
	attached  atomic.Bool
	// single is a run of one task; flowed is set once a flow took -from
	// or -until.
	single bool
	flowed atomic.Bool
	// slots hold a place for each browser rented, when the program limits
	// how many it has at once.
	slots chan struct{}

	storeOnce sync.Once
	store     *store.Store
	storeErr  error
}

// proxyTries is how many proxies one rent goes through before the job counts
// as a try that failed.
const proxyTries = 3

// rentWaits say why a rent waits and for how long before asking again.
var rentWaits = map[string]struct {
	text string
	d    time.Duration
}{
	"NO_CAPACITY_AVAILABLE":    {"no browser is free right now", 3 * time.Second},
	"SESSION_LIMIT_REACHED":    {"the account runs as many browsers as it may", 10 * time.Second},
	"BROWSER_CREATION_FAILED":  {"the browser didn't start", 5 * time.Second},
	"PROXY_SERVER_UNAVAILABLE": {"the managed proxy service didn't answer", 5 * time.Second},
	"PROXY_GENERATION_FAILED":  {"no managed proxy could be allocated", 5 * time.Second},
}

var rentFatal = map[string]string{
	"INVALID_API_KEY":          "browserscale rejected the API key.",
	"INSUFFICIENT_CREDITS":     "The browserscale account has no credits left.",
	"INSUFFICIENT_PROXY_QUOTA": "The browserscale account's proxy traffic is used up.",
}

// slot takes a place for the job's browser when the program limits them.
func (rn *runner) slot(j *Job) error {
	if rn.slots == nil || j.slot {
		return nil
	}
	select {
	case rn.slots <- struct{}{}:
	default:
		j.emit(Event{Type: "wait", Text: fmt.Sprintf("all %d browsers the program may have are in use", cap(rn.slots))})
		select {
		case rn.slots <- struct{}{}:
		case <-j.Ctx.Done():
			return j.Ctx.Err()
		}
	}
	j.slot = true
	return nil
}

func (rn *runner) browserFor(j *Job) error {
	if err := rn.slot(j); err != nil {
		return err
	}
	if rn.opts.Attach != "" && rn.attached.CompareAndSwap(false, true) {
		b, err := rn.h.attach(j.Ctx, rn.opts.Grpc, rn.key, rn.opts.Attach)
		if err != nil {
			return &fatalError{msg: fmt.Sprintf("Couldn't attach to the browser %s: %v", rn.opts.Attach, err)}
		}
		j.browser, j.attached = b, true
		j.emit(Event{Type: "browser", Data: map[string]any{"session": rn.opts.Attach, "attached": true}})
		return nil
	}
	if rn.key == "" {
		return &fatalError{msg: "There's no browserscale API key: set BROWSERSCALE_API_KEY, or add an apiKey setting."}
	}
	// A proxy the job took with Proxy stays its proxy: the browser goes out
	// where its requests did.
	pinned := j.proxy != nil && j.addr != nil
	badProxies, failures := 0, 0
	told := map[string]bool{}
	for {
		e, p := j.proxy, proxy.Proxy{}
		if pinned {
			p = *j.addr
		} else {
			var err error
			if e, p, err = rn.takeProxy(j); err != nil {
				return err
			}
		}
		cfg := browserscale.NewBrowserConfig(rn.key, int(rn.lease.Seconds()), p.Host, p.Port, p.Username, p.Password)
		if scheme, _ := p.BrowserScheme(); e != nil && scheme == "socks5" {
			cfg.WithProxyScheme(browserscale.ProxySOCKS5)
		}
		if e != nil && e.e.Masque != "" {
			cfg.WithMasque(e.e.Masque)
		}
		if e == nil && rn.country != "" {
			cfg.WithCountryCode(strings.ToUpper(rn.country))
		}
		if rn.configure != nil {
			rn.configure(cfg)
		}
		b, err := rn.h.rent(j.Ctx, cfg)
		if err == nil {
			j.browser, j.proxy = b, e
			if e != nil {
				j.addr = &p
			}
			id, _ := rn.h.session(b)
			data := map[string]any{"session": id}
			if e != nil {
				data["proxy"] = e.label()
			}
			j.emit(Event{Type: "browser", Data: data})
			return nil
		}
		if j.Ctx.Err() != nil {
			return j.Ctx.Err()
		}
		code := rentCode(err)
		if msg, ok := rentFatal[code]; ok {
			return &fatalError{msg: msg}
		}
		if w, ok := rentWaits[code]; ok {
			if !told[code] {
				j.emit(Event{Type: "wait", Text: w.text})
				told[code] = true
			}
			if err := rn.h.sleep(j.Ctx, w.d); err != nil {
				return err
			}
			continue
		}
		if code == "PROXY_CONNECTION_FAILED" && e != nil {
			if pinned {
				return &retryError{reason: fmt.Sprintf("the job's proxy %s didn't connect to start a browser", e.label()), newProxy: true}
			}
			what := rn.proxies.rest(e)
			j.emit(Event{Type: "wait", Text: fmt.Sprintf("proxy %s didn't connect; it %s", e.label(), what)})
			if badProxies++; badProxies >= rn.proxies.tries() || j.proxyID != "" {
				return &retryError{reason: "no proxy got through to start a browser", newProxy: true}
			}
			continue
		}
		if failures++; failures >= 3 {
			return &retryError{reason: "couldn't rent a browser: " + err.Error()}
		}
		if err := rn.h.sleep(j.Ctx, 5*time.Second); err != nil {
			return err
		}
	}
}

// takeProxy picks the job's proxy from the run's, gets a mobile one a new
// address when it asked for one, and fills in the placeholders. e is nil
// when the run uses browserscale's managed proxies.
func (rn *runner) takeProxy(j *Job) (e *proxyEntry, p proxy.Proxy, err error) {
	if rn.proxies == nil {
		if j.proxyID != "" {
			return nil, p, &fatalError{msg: fmt.Sprintf("Job %d asks for the proxy %s, but the run uses browserscale's managed proxies.", j.ID, j.proxyID)}
		}
		return nil, p, nil
	}
	if j.proxyID != "" {
		if e, err = rn.proxies.byID(j.proxyID); err != nil {
			return nil, p, &fatalError{msg: fmt.Sprintf("Job %d asks for a proxy the run doesn't have: %v.", j.ID, err)}
		}
	} else {
		e, err = rn.proxies.take(j.Ctx, func(d time.Duration) {
			j.emit(Event{Type: "wait", Text: "every proxy rests after failing; the next one is back in " + d.Round(time.Second).String()})
		})
		if err != nil {
			return nil, p, err
		}
	}
	if rn.proxies.rotation(e) {
		j.emit(Event{Type: "wait", Text: fmt.Sprintf("proxy %s gets a new address", e.label())})
		if err := e.e.Rotate(j.Ctx); err != nil {
			if j.Ctx.Err() != nil {
				return nil, p, j.Ctx.Err()
			}
			j.emit(Event{Type: "log", Text: fmt.Sprintf("proxy %s didn't rotate: %v", e.label(), err)})
		}
	}
	if p, err = e.e.Render(); err != nil {
		return nil, p, &fatalError{msg: fmt.Sprintf("Proxy %s can't be used: %v.", e.label(), err)}
	}
	return e, p, nil
}

type outcome struct {
	end, detail, code, shot string
	tone                    Tone
	newProxy                bool
	unexpected              bool
	// site is an end the job reached itself, so the proxy got it through;
	// paused a job stopped after the step Options.Until names.
	site, paused bool

	// again asks for another try, at most limit; giveUp is the end once
	// they ran out.
	again  bool
	limit  int
	why    string
	giveUp string
}

func (o outcome) gaveUp(ends Ends) outcome {
	o.end = o.giveUp
	o.tone = ends.of(o.giveUp).tone
	if o.detail == "" {
		o.detail = o.why
	}
	o.again = false
	return o
}

// judge sorts out how a try ended.
func (rn *runner) judge(j *Job, err error) outcome {
	if err == nil {
		name, detail := EndDone, ""
		if j.ended != nil {
			name, detail = j.ended.name, j.ended.detail
		}
		p := j.ends.of(name)
		o := outcome{end: name, detail: detail, tone: p.tone, newProxy: p.changeProxy, site: true}
		if p.retries > 0 {
			o.again, o.limit, o.why, o.giveUp = true, p.retries, "it ended as "+name, name
		}
		return o
	}
	stopped := j.ends.of(EndStopped).tone
	var fe *fatalError
	if errors.As(err, &fe) {
		rn.core.Fail(fe)
		return outcome{end: EndStopped, tone: stopped, detail: fe.msg}
	}
	var pause *pauseError
	if errors.As(err, &pause) {
		rn.core.Stop(fmt.Sprintf("job %d stopped after %s, as -until asked", j.ID, pause.step))
		return outcome{end: EndStopped, tone: stopped, detail: "after " + pause.step, paused: true}
	}
	if rn.core.ctx.Err() != nil {
		return outcome{end: EndStopped, tone: stopped}
	}
	var re *retryError
	if errors.As(err, &re) {
		o := outcome{again: true, limit: rn.retries, why: re.reason, newProxy: re.newProxy, giveUp: EndGaveUp}
		if re.newProxy {
			o.giveUp = EndProxyFailed
		}
		return o
	}
	if proxyFault(err) {
		return outcome{again: true, limit: rn.retries, why: "the page didn't load: " + err.Error(), newProxy: true, giveUp: EndProxyFailed}
	}
	if transportFault(err) {
		return outcome{again: true, limit: rn.retries, why: "the connection to the browser broke: " + err.Error(), giveUp: EndGaveUp}
	}
	_, code, _ := sdkCode(err)
	p := j.ends.of(EndError)
	o := outcome{end: EndError, tone: p.tone, detail: err.Error(), code: code, unexpected: true, newProxy: p.changeProxy}
	var pe *panicError
	if errors.As(err, &pe) {
		j.emit(Event{Type: "log", Text: string(pe.stack)})
	}
	if p.retries > 0 {
		o.again, o.limit, o.why, o.giveUp = true, p.retries, err.Error(), EndError
	}
	return o
}

// release lets the try's proxy rest when it was at fault, keeps a picture of
// what went wrong, and gives back the browser.
func (rn *runner) release(j *Job, o outcome) outcome {
	if j.proxy != nil {
		if o.newProxy {
			what := rn.proxies.rest(j.proxy)
			j.emit(Event{Type: "wait", Text: fmt.Sprintf("proxy %s %s", j.proxy.label(), what)})
		} else if o.site && !o.again {
			rn.proxies.worked(j.proxy)
		}
	}
	if o.unexpected && j.page != nil {
		o.shot = rn.screenshot(j)
	}
	if j.slot {
		defer func() {
			<-rn.slots
			j.slot = false
		}()
	}
	if j.browser == nil {
		return o
	}
	keep := j.attached || (rn.opts.Hold && ((o.unexpected && !o.again) || o.paused))
	if keep && (o.unexpected || o.paused) {
		id, grpc := rn.h.session(j.browser)
		j.emit(Event{Type: "held", Data: map[string]any{"session": id, "grpc": grpc}})
		if !j.attached && o.unexpected {
			rn.core.Stop(fmt.Sprintf("job %d failed at %s; its browser keeps running to look at", j.ID, or(j.step, "its start")))
		}
	}
	rn.h.release(j.browser, keep)
	j.browser, j.page = nil, nil
	return o
}

func (rn *runner) screenshot(j *Job) string {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	data, err := rn.h.shot(ctx, j.page)
	if err != nil || len(data) == 0 {
		return ""
	}
	dir := filepath.Join(rn.runDir, "shots")
	if os.MkdirAll(dir, 0o755) != nil {
		return ""
	}
	name := fmt.Sprintf("%d-%d.jpg", j.ID, j.Try)
	if os.WriteFile(filepath.Join(dir, name), data, 0o644) != nil {
		return ""
	}
	return "shots/" + name
}

func (rn *runner) end(j *Job, o outcome, tries int, took time.Duration) {
	secs := math.Round(took.Seconds()*10) / 10
	data := map[string]any{"seconds": secs}
	if o.code != "" {
		data["code"] = o.code
	}
	if o.shot != "" {
		data["shot"] = o.shot
	}
	e := Event{Type: "end", End: o.end, Tone: o.tone, Text: o.detail, Data: data}
	e.Job, e.Worker, e.Try, e.Step, e.Task = j.ID, j.Worker, j.Try, j.step, j.Task
	e.At = time.Now().UnixMilli()
	rn.ev.emit(e)
	rn.ev.row(e, tries, secs)
	if j.run.finish(o.end, o.tone, j.step, rn.stopAfter) {
		jobs := "jobs"
		if j.Task != "" {
			jobs = j.Task + " jobs"
		}
		reason := fmt.Sprintf("%d %s in a row failed at %s; the site may have changed", rn.stopAfter, jobs, or(j.step, "the start"))
		if o.end == EndProxyFailed {
			reason = fmt.Sprintf("%d %s in a row got no proxy through; check the proxies", rn.stopAfter, jobs)
		}
		rn.core.Stop(reason)
	}
}

func (rn *runner) namespace() (*store.Namespace, error) {
	rn.storeOnce.Do(func() {
		rn.store, rn.storeErr = store.Open(filepath.Join(rn.dataDir, "store.db"))
	})
	if rn.storeErr != nil {
		return nil, rn.storeErr
	}
	return rn.store.Namespace(rn.name), nil
}

func (rn *runner) closeStore() {
	if rn.store != nil {
		_ = rn.store.Close()
	}
}
