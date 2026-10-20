package kit

import (
	"bufio"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	browserscale "github.com/browserscale/browserscale-go"
	"github.com/browserscale/browserscale-kit/form"
	"github.com/browserscale/browserscale-kit/proxy"
)

type fakeBrowserscale struct {
	mu       sync.Mutex
	rents    []error // what each rent returns, nil for a browser; a browser once they run out
	rented   int
	released []bool
}

func (f *fakeBrowserscale) hooks() hooks {
	return hooks{
		rent: func(ctx context.Context, c *browserscale.BrowserConfig) (*browserscale.CloudBrowser, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.rented++
			if len(f.rents) > 0 {
				err := f.rents[0]
				f.rents = f.rents[1:]
				if err != nil {
					return nil, err
				}
			}
			return &browserscale.CloudBrowser{}, nil
		},
		proxyOf: func(context.Context, *browserscale.CloudBrowser) (proxy.Proxy, error) {
			return proxy.Proxy{Host: "managed.example", Port: 9}, nil
		},
		attach: func(ctx context.Context, grpc, key, session string) (*browserscale.CloudBrowser, error) {
			return &browserscale.CloudBrowser{}, nil
		},
		release: func(b *browserscale.CloudBrowser, keep bool) {
			f.mu.Lock()
			f.released = append(f.released, keep)
			f.mu.Unlock()
		},
		session: func(*browserscale.CloudBrowser) (string, string) { return "s1", "grpc://x" },
		newPage: func(context.Context, *browserscale.CloudBrowser, bool) (*browserscale.Page, error) {
			return &browserscale.Page{}, nil
		},
		shot:  func(context.Context, *browserscale.Page) ([]byte, error) { return []byte("jpeg"), nil },
		sleep: func(context.Context, time.Duration) error { return nil },
	}
}

type result struct {
	rep    *Report
	err    error
	events []Event
	dir    string
}

func (r result) types(job int) []string {
	var out []string
	for _, e := range r.events {
		if e.Job == job {
			out = append(out, e.Type)
		}
	}
	return out
}

func (r result) ends() []Event {
	var out []Event
	for _, e := range r.events {
		if e.Type == "end" {
			out = append(out, e)
		}
	}
	return out
}

func runBot[S, J any](t *testing.T, b Bot[S, J], o Options, f *fakeBrowserscale, files map[string]string) result {
	t.Helper()
	return runProgram(t, b.program(), o, f, files)
}

func runProgram[S any](t *testing.T, p Program[S], o Options, f *fakeBrowserscale, files map[string]string) result {
	t.Helper()
	root := t.TempDir()
	o.Data, o.Runs, o.Run, o.Yes = filepath.Join(root, "data"), filepath.Join(root, "runs"), "test", true
	if err := os.MkdirAll(o.Data, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(o.Data, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if f == nil {
		f = &fakeBrowserscale{}
	}
	rep, err := p.start(context.Background(), o, f.hooks())
	dir := filepath.Join(o.Runs, "test")
	r := result{rep: rep, err: err, dir: dir}
	file, ferr := os.Open(filepath.Join(dir, "events.jsonl"))
	if ferr != nil {
		return r
	}
	defer file.Close()
	sc := bufio.NewScanner(file)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var e Event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("event %q: %v", sc.Text(), err)
		}
		r.events = append(r.events, e)
	}
	return r
}

func readCSV(t *testing.T, path string) [][]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rows, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

type none struct{}

func TestEndsCountAndReport(t *testing.T) {
	b := Bot[none, int]{
		Jobs: func(r *Run, s *none) iter.Seq[int] { return Times(4, func(i int) int { return i }) },
		Job: func(j *Job, s *none, i int) error {
			switch i {
			case 0:
				j.End("blocked", "riskBlock")
			case 2:
				j.End("phone_required")
			case 3:
				return errors.New("boom")
			}
			return nil
		},
		Ends: Ends{"blocked": Bad, "phone_required": Neutral},
	}
	r := runBot(t, b, Options{}, nil, nil)
	if r.err != nil {
		t.Fatal(r.err)
	}
	if r.rep.Tones[ToneGood] != 1 || r.rep.Tones[ToneNeutral] != 1 || r.rep.Tones[ToneBad] != 2 || r.rep.Ends["error"] != 1 {
		t.Fatalf("report = %+v", r.rep)
	}
	declared, _ := json.Marshal(r.events[0].Data["ends"])
	if r.events[0].Type != "run" || string(declared) != `[{"name":"phone_required","tone":"neutral"},{"name":"blocked","tone":"bad"}]` {
		t.Fatalf("run event = %+v", r.events[0])
	}
	rows := readCSV(t, filepath.Join(r.dir, "status.csv"))
	if len(rows) != 5 || rows[0][1] != "end" {
		t.Fatalf("status = %v", rows)
	}
	byEnd := map[string][]string{}
	for _, row := range rows[1:] {
		byEnd[row[1]] = row
	}
	if byEnd["blocked"][4] != "riskBlock" || byEnd["error"][4] != "boom" || byEnd["done"][2] != "good" {
		t.Fatalf("status rows = %v", byEnd)
	}
	if last := r.events[len(r.events)-1]; last.Type != "done" {
		t.Fatalf("last event = %+v", last)
	}
}

type flowBot struct {
	*Job
	trail *[]string
}

func (f *flowBot) open() (Step, error) {
	*f.trail = append(*f.trail, "open")
	return f.fillEmail, nil
}

func (f *flowBot) fillEmail() (Step, error) {
	*f.trail = append(*f.trail, "fillEmail")
	// What Wait returns when a watched condition matched first.
	return nil, &watchHit{then: f.captcha, to: "captcha"}
}

func (f *flowBot) captcha() (Step, error) {
	*f.trail = append(*f.trail, "captcha")
	return f.End("captcha_reached")
}

func TestFlowStepsWatchAndFrom(t *testing.T) {
	var trails [][]string
	var mu sync.Mutex
	b := Bot[none, none]{
		Jobs: func(r *Run, s *none) iter.Seq[none] { return Times[none](2, nil) },
		Job: func(j *Job, s *none, _ none) error {
			var trail []string
			f := &flowBot{Job: j, trail: &trail}
			err := j.Flow(f.open, f.fillEmail, f.captcha)
			mu.Lock()
			trails = append(trails, trail)
			mu.Unlock()
			return err
		},
		Ends: Ends{"captcha_reached": Good},
	}
	r := runBot(t, b, Options{From: "fill_email"}, nil, nil)
	if r.err != nil || r.rep.Ends["captcha_reached"] != 2 {
		t.Fatalf("report = %+v, %v", r.rep, r.err)
	}
	if strings.Join(trails[0], ">") != "fillEmail>captcha" || strings.Join(trails[1], ">") != "open>fillEmail>captcha" {
		t.Fatalf("trails = %v", trails)
	}
	var steps []string
	for _, e := range r.events {
		if e.Type == "step" && e.Job == 2 {
			steps = append(steps, e.Step)
		}
	}
	if strings.Join(steps, ">") != "open>fillEmail>captcha" {
		t.Fatalf("step events = %v", steps)
	}
	if end := r.ends()[0]; end.Step != "captcha" || end.End != "captcha_reached" {
		t.Fatalf("end = %+v", end)
	}
}

func TestFromUnknownStep(t *testing.T) {
	b := Bot[none, none]{Job: func(j *Job, s *none, _ none) error {
		f := &flowBot{Job: j, trail: new([]string)}
		return j.Flow(f.open, f.captcha)
	}}
	r := runBot(t, b, Options{From: "nope"}, nil, nil)
	if e := r.ends()[0]; e.End != EndError || !strings.Contains(e.Text, "open, captcha") {
		t.Fatalf("end = %+v", e)
	}
}

type proxied struct {
	Proxies string `json:"proxies"`
}

func proxiedSettings(f *form.Form, s *proxied) {
	f.File("proxies", &s.Proxies).Tag(form.TagProxies).Default("proxies.txt")
}

func TestRentWaitsAndRotatesProxies(t *testing.T) {
	t.Setenv("BROWSERSCALE_API_KEY", "k")
	fb := &fakeBrowserscale{rents: []error{
		errors.New("rent session failed: PROXY_CONNECTION_FAILED"),
		errors.New("rent session failed: NO_CAPACITY_AVAILABLE"),
		nil,
	}}
	b := Bot[proxied, none]{
		Settings: proxiedSettings,
		Job: func(j *Job, s *proxied, _ none) error {
			_, err := j.Browser()
			return err
		},
	}
	r := runBot(t, b, Options{}, fb, map[string]string{"proxies.txt": "a.example:1000:u:p\nb.example:2000:u:p\n"})
	if r.err != nil || r.rep.Ends[EndDone] != 1 || fb.rented != 3 {
		t.Fatalf("report = %+v, %v, rented %d", r.rep, r.err, fb.rented)
	}
	var waits []string
	var proxy any
	for _, e := range r.events {
		switch e.Type {
		case "wait":
			waits = append(waits, e.Text)
		case "browser":
			proxy = e.Data["proxy"]
		}
	}
	if len(waits) != 2 || !strings.Contains(waits[0], "a.example:1000") || !strings.Contains(waits[1], "no browser is free") {
		t.Fatalf("waits = %q", waits)
	}
	if proxy != "b.example:2000" {
		t.Fatalf("proxy = %v", proxy)
	}
	if len(fb.released) != 1 || fb.released[0] {
		t.Fatalf("released = %v", fb.released)
	}
}

func TestSessionToken(t *testing.T) {
	src, err := proxy.Spec{Kind: proxy.KindList, Lines: []string{"h.example:1:user:pw_session-{session}_lifetime-3m"}}.Source("", "")
	if err != nil {
		t.Fatal(err)
	}
	e := src.Entries[0]
	a, _ := e.Render()
	b, _ := e.Render()
	if !e.Rotating || a.Password == b.Password || strings.Contains(a.Password, "{session}") || !strings.HasPrefix(a.Password, "pw_session-") || !strings.HasSuffix(a.Password, "_lifetime-3m") {
		t.Fatalf("passwords %q %q", a.Password, b.Password)
	}
}

func TestRotatingProxyStaysIn(t *testing.T) {
	src, err := proxy.Spec{Kind: proxy.KindGateway, Name: "gw", Template: "gw.example:7000:u:p_session-{session}"}.Source("", "")
	if err != nil {
		t.Fatal(err)
	}
	ps := newProxies(src)
	e := ps.list[0]
	for i := 0; i < rotatingGrace; i++ {
		if what := ps.rest(e); !strings.Contains(what, "new address") {
			t.Fatalf("strike %d: %s", i+1, what)
		}
	}
	if what := ps.rest(e); what != "rests 1m0s" {
		t.Fatalf("after grace: %s", what)
	}
	if ps.tries() != proxyTries {
		t.Fatalf("tries = %d", ps.tries())
	}
}

func TestUseProxyPicksStaticEntry(t *testing.T) {
	t.Setenv("BROWSERSCALE_API_KEY", "k")
	dir := t.TempDir()
	t.Setenv(proxy.SourcesEnv, dir)
	spec := `{"kind":"static","name":"Accounts","entries":[{"id":"a1","line":"a.example:1:u:p"},{"id":"b2","line":"b.example:2:u:p"}]}`
	if err := os.WriteFile(filepath.Join(dir, "acc.json"), []byte(spec), 0o644); err != nil {
		t.Fatal(err)
	}
	var got []string
	b := Bot[proxied, none]{
		Settings: proxiedSettings,
		Jobs:     func(r *Run, s *proxied) iter.Seq[none] { return Times[none](2, nil) },
		Job: func(j *Job, s *proxied, _ none) error {
			j.UseProxy("b2")
			if _, err := j.Browser(); err != nil {
				return err
			}
			got = append(got, j.ProxyID())
			return nil
		},
	}
	fb := &fakeBrowserscale{}
	r := runBot(t, b, Options{}, fb, map[string]string{"config.json": `{"proxies":"proxy:acc"}`})
	if r.rep.Ends[EndDone] != 2 || strings.Join(got, ",") != "b2,b2" {
		t.Fatalf("report %+v, ids %v, err %v", r.rep, got, r.err)
	}
}

const sessionProxies = "a.example:1000:u:pw_session-{session}\nb.example:2000:u:pw_session-{session}\n"

func TestProxyWithoutBrowser(t *testing.T) {
	t.Setenv("BROWSERSCALE_API_KEY", "k")
	var seen []string
	b := Bot[proxied, none]{
		Settings: proxiedSettings,
		Job: func(j *Job, s *proxied, _ none) error {
			p, err := j.Proxy()
			if err != nil {
				return err
			}
			again, _ := j.Proxy()
			seen = append(seen, p.URL(), again.URL())
			if j.Try == 1 {
				return j.BadProxy("the API turned the address away")
			}
			if _, err := j.Browser(); err != nil {
				return err
			}
			after, _ := j.Proxy()
			seen = append(seen, after.URL())
			return nil
		},
	}
	fb := &fakeBrowserscale{}
	r := runBot(t, b, Options{}, fb, map[string]string{"proxies.txt": sessionProxies})
	if r.err != nil || r.rep.Ends[EndDone] != 1 || fb.rented != 1 || len(seen) != 5 {
		t.Fatalf("report %+v, err %v, rented %d, seen %v", r.rep, r.err, fb.rented, seen)
	}
	if seen[0] != seen[1] || strings.Contains(seen[0], "{session}") || !strings.Contains(seen[0], "a.example") {
		t.Fatalf("first try = %v", seen[:2])
	}
	if seen[2] != seen[3] || seen[3] != seen[4] || !strings.Contains(seen[2], "b.example") {
		t.Fatalf("second try = %v", seen[2:])
	}
	var browserProxy any
	for _, e := range r.events {
		if e.Type == "browser" {
			browserProxy = e.Data["proxy"]
		}
	}
	if browserProxy != "b.example:2000" {
		t.Fatalf("browser went through %v", browserProxy)
	}
}

func TestProxyOfManagedComesWithBrowser(t *testing.T) {
	t.Setenv("BROWSERSCALE_API_KEY", "k")
	var before error
	var after proxy.Proxy
	b := Bot[none, none]{
		Job: func(j *Job, s *none, _ none) error {
			_, before = j.Proxy()
			if _, err := j.Browser(); err != nil {
				return err
			}
			var err error
			after, err = j.Proxy()
			return err
		},
	}
	r := runBot(t, b, Options{}, nil, nil)
	if !errors.Is(before, ErrManagedProxy) || after.Host != "managed.example" || r.rep.Ends[EndDone] != 1 {
		t.Fatalf("before %v, after %+v, report %+v", before, after, r.rep)
	}
}

func TestCloseBrowserKeepsProxy(t *testing.T) {
	t.Setenv("BROWSERSCALE_API_KEY", "k")
	var urls []string
	b := Bot[proxied, none]{
		Settings: proxiedSettings,
		Browsers: func(s *proxied) int { return 1 },
		Job: func(j *Job, s *proxied, _ none) error {
			for range 2 {
				if _, err := j.Page(); err != nil {
					return err
				}
				p, _ := j.Proxy()
				urls = append(urls, p.URL())
				j.CloseBrowser()
			}
			return nil
		},
	}
	fb := &fakeBrowserscale{}
	r := runBot(t, b, Options{}, fb, map[string]string{"proxies.txt": sessionProxies})
	if r.rep.Ends[EndDone] != 1 || fb.rented != 2 || len(fb.released) != 2 || len(urls) != 2 || urls[0] != urls[1] {
		t.Fatalf("report %+v, err %v, rented %d, released %v, urls %v", r.rep, r.err, fb.rented, fb.released, urls)
	}
}

func TestPinnedProxyThatFailsTriesAnother(t *testing.T) {
	t.Setenv("BROWSERSCALE_API_KEY", "k")
	fb := &fakeBrowserscale{rents: []error{errors.New("rent session failed: PROXY_CONNECTION_FAILED")}}
	var used []string
	b := Bot[proxied, none]{
		Settings: proxiedSettings,
		Job: func(j *Job, s *proxied, _ none) error {
			p, err := j.Proxy()
			if err != nil {
				return err
			}
			used = append(used, p.Host)
			_, err = j.Browser()
			return err
		},
	}
	r := runBot(t, b, Options{}, fb, map[string]string{"proxies.txt": sessionProxies})
	if r.rep.Ends[EndDone] != 1 || fb.rented != 2 || strings.Join(used, ",") != "a.example,b.example" {
		t.Fatalf("report %+v, err %v, rented %d, used %v", r.rep, r.err, fb.rented, used)
	}
}

func TestFatalRentStopsRun(t *testing.T) {
	t.Setenv("BROWSERSCALE_API_KEY", "k")
	fb := &fakeBrowserscale{rents: []error{errors.New("rent session failed: INVALID_API_KEY")}}
	b := Bot[none, none]{
		Jobs: func(r *Run, s *none) iter.Seq[none] { return Times[none](3, nil) },
		Job: func(j *Job, s *none, _ none) error {
			_, err := j.Browser()
			return err
		},
	}
	r := runBot(t, b, Options{}, fb, nil)
	if r.err == nil || !strings.Contains(r.err.Error(), "API key") {
		t.Fatalf("err = %v", r.err)
	}
	if ends := r.ends(); len(ends) != 1 || ends[0].End != EndStopped {
		t.Fatalf("ends = %+v", ends)
	}
}

func TestBadProxyRetriesThenGivesUp(t *testing.T) {
	t.Setenv("BROWSERSCALE_API_KEY", "k")
	tries := 0
	b := Bot[proxied, none]{
		Settings: proxiedSettings,
		Job: func(j *Job, s *proxied, _ none) error {
			tries++
			if _, err := j.Browser(); err != nil {
				return err
			}
			return j.BadProxy("the site turned the address away")
		},
	}
	fb := &fakeBrowserscale{}
	r := runBot(t, b, Options{}, fb, map[string]string{"proxies.txt": "a.example:1:u:p\nb.example:2:u:p\nc.example:3:u:p\n"})
	e := r.ends()[0]
	if tries != 3 || e.End != EndProxyFailed || e.Text != "the site turned the address away" || len(fb.released) != 3 {
		t.Fatalf("tries %d, end %+v, released %v", tries, e, fb.released)
	}
	if got := r.types(1); strings.Count(strings.Join(got, ","), "retry") != 2 {
		t.Fatalf("events = %v", got)
	}
}

func TestEndPolicyRetriesWithNewProxy(t *testing.T) {
	t.Setenv("BROWSERSCALE_API_KEY", "k")
	var used []string
	b := Bot[proxied, none]{
		Settings: proxiedSettings,
		Job: func(j *Job, s *proxied, _ none) error {
			if _, err := j.Browser(); err != nil {
				return err
			}
			used = append(used, j.proxy.label())
			if j.Try < 2 {
				j.End("ip_blocked")
			}
			return nil
		},
		Ends: Ends{"ip_blocked": Bad.Retry(3).ChangeProxy()},
	}
	r := runBot(t, b, Options{}, nil, map[string]string{"proxies.txt": "a.example:1\nb.example:2\n"})
	if r.rep.Ends[EndDone] != 1 || strings.Join(used, ",") != "a.example:1,b.example:2" {
		t.Fatalf("report %+v, used %v", r.rep, used)
	}
}

func TestStopAfterSameStepFails(t *testing.T) {
	b := Bot[none, none]{
		Jobs: func(r *Run, s *none) iter.Seq[none] { return Forever[none](nil) },
		Job: func(j *Job, s *none, _ none) error {
			f := &flowBot{Job: j, trail: new([]string)}
			return j.Flow(func() (Step, error) { return f.captchaFails, nil })
		},
		StopAfter: 3,
	}
	r := runBot(t, b, Options{}, nil, nil)
	if len(r.ends()) != 3 || !strings.Contains(r.rep.Stopped, "captchaFails") {
		t.Fatalf("ends %d, report %+v", len(r.ends()), r.rep)
	}
}

func (f *flowBot) captchaFails() (Step, error) { return nil, errors.New("no challenge") }

func TestUnexpectedKeepsShotAndHolds(t *testing.T) {
	t.Setenv("BROWSERSCALE_API_KEY", "k")
	fb := &fakeBrowserscale{}
	b := Bot[none, none]{
		Jobs: func(r *Run, s *none) iter.Seq[none] { return Times[none](3, nil) },
		Job: func(j *Job, s *none, _ none) error {
			if _, err := j.Page(); err != nil {
				return err
			}
			return &browserscale.WaitError{Code: "timeout", Message: "nothing matched"}
		},
	}
	r := runBot(t, b, Options{Hold: true}, fb, nil)
	e := r.ends()[0]
	if len(r.ends()) != 1 || e.End != EndError || e.Data["code"] != "timeout" || e.Data["shot"] != "shots/1-1.jpg" {
		t.Fatalf("ends = %+v", r.ends())
	}
	if _, err := os.Stat(filepath.Join(r.dir, "shots", "1-1.jpg")); err != nil {
		t.Fatal(err)
	}
	if len(fb.released) != 1 || !fb.released[0] || !strings.Contains(strings.Join(r.types(1), ","), "held") {
		t.Fatalf("released %v, events %v", fb.released, r.types(1))
	}
}

func TestAttachedBrowserKeepsRunning(t *testing.T) {
	t.Setenv("BROWSERSCALE_API_KEY", "k")
	fb := &fakeBrowserscale{}
	b := Bot[none, none]{Job: func(j *Job, s *none, _ none) error {
		_, err := j.Page()
		return err
	}}
	r := runBot(t, b, Options{Attach: "s9", Grpc: "grpc://x"}, fb, nil)
	if r.rep.Ends[EndDone] != 1 || fb.rented != 0 || len(fb.released) != 1 || !fb.released[0] {
		t.Fatalf("report %+v, rented %d, released %v", r.rep, fb.rented, fb.released)
	}
}

func TestPanicEndsAsError(t *testing.T) {
	b := Bot[none, none]{Job: func(j *Job, s *none, _ none) error {
		var m map[string]int
		m["x"] = 1
		return nil
	}}
	r := runBot(t, b, Options{}, nil, nil)
	if e := r.ends()[0]; e.End != EndError || !strings.Contains(e.Text, "panic") {
		t.Fatalf("end = %+v", e)
	}
}

func TestUntilStopsAtTarget(t *testing.T) {
	var mu sync.Mutex
	n := 0
	b := Bot[none, int]{
		Jobs:    func(r *Run, s *none) iter.Seq[int] { return Until(r, 3, func(i int) int { return i }) },
		Workers: func(*none) int { return 4 },
		Job: func(j *Job, s *none, i int) error {
			mu.Lock()
			n++
			mu.Unlock()
			if i%2 == 1 {
				j.End("blocked")
			}
			return nil
		},
	}
	r := runBot(t, b, Options{}, nil, nil)
	if r.rep.Ends[EndDone] != 3 || r.rep.Ends["blocked"] != n-3 {
		t.Fatalf("report %+v after %d jobs", r.rep, n)
	}
}

func TestUntilCountsNamedEnds(t *testing.T) {
	b := Bot[none, int]{
		Jobs: func(r *Run, s *none) iter.Seq[int] {
			return Until(r, 2, func(i int) int { return i }, "changed")
		},
		Job: func(j *Job, s *none, i int) error {
			if i%3 == 2 {
				j.End("changed")
			} else {
				j.End("unchanged")
			}
			return nil
		},
		Ends: Ends{"changed": Neutral, "unchanged": Neutral},
	}
	r := runBot(t, b, Options{}, nil, nil)
	if r.rep.Ends["changed"] != 2 || r.rep.Ends["unchanged"] != 4 || r.rep.Tones[ToneGood] != 0 {
		t.Fatalf("report %+v", r.rep)
	}
}

func TestUntilStopsAfterTheStep(t *testing.T) {
	t.Setenv("BROWSERSCALE_API_KEY", "k")
	fb := &fakeBrowserscale{}
	var trail []string
	b := Bot[none, none]{
		Jobs: func(r *Run, s *none) iter.Seq[none] { return Times[none](3, nil) },
		Job: func(j *Job, s *none, _ none) error {
			if _, err := j.Page(); err != nil {
				return err
			}
			f := &flowBot{Job: j, trail: &trail}
			return j.Flow(f.open, f.fillEmail, f.captcha)
		},
	}
	r := runBot(t, b, Options{Until: "open", Hold: true}, fb, nil)
	e := r.ends()
	if r.err != nil || len(e) != 1 || e[0].End != EndStopped || e[0].Text != "after open" || strings.Join(trail, ">") != "open" {
		t.Fatalf("ends %+v, trail %v, %v", e, trail, r.err)
	}
	if !strings.Contains(r.rep.Stopped, "after open") || len(fb.released) != 1 || !fb.released[0] {
		t.Fatalf("report %+v, released %v", r.rep, fb.released)
	}
	r = runBot(t, b, Options{Until: "nope"}, nil, nil)
	if e := r.ends(); len(e) == 0 || e[0].End != EndError || !strings.Contains(e[0].Text, `no step "nope"`) {
		t.Fatalf("ends %+v", e)
	}
}

type account struct {
	Email     string `csv:"email"`
	FirstName string
	Age       int
	Secret    string `csv:"-"`
}

func TestRowsAndSave(t *testing.T) {
	b := Bot[none, account]{
		Jobs: func(r *Run, s *none) iter.Seq[account] { return Rows[account](r, "accounts.csv") },
		Job: func(j *Job, s *none, a account) error {
			a.Secret = "x"
			if err := j.Save("out", a); err != nil {
				return err
			}
			return j.Save("emails.txt", a.Email)
		},
	}
	r := runBot(t, b, Options{}, nil, map[string]string{"accounts.csv": "Email,first_name,age\na@x.com,Ann,30\nb@x.com,Bob,41\n"})
	if r.err != nil || r.rep.Ends[EndDone] != 2 {
		t.Fatalf("report %+v, %v", r.rep, r.err)
	}
	rows := readCSV(t, filepath.Join(r.dir, "out.csv"))
	if len(rows) != 3 || strings.Join(rows[0], ",") != "email,FirstName,Age" || strings.Join(rows[2], ",") != "b@x.com,Bob,41" {
		t.Fatalf("out = %v", rows)
	}
	text, _ := os.ReadFile(filepath.Join(r.dir, "emails.txt"))
	if string(text) != "a@x.com\nb@x.com\n" {
		t.Fatalf("emails = %q", text)
	}
}

func TestRowsMissingFileFailsRun(t *testing.T) {
	b := Bot[none, account]{
		Jobs: func(r *Run, s *none) iter.Seq[account] { return Rows[account](r, "missing.csv") },
		Job:  func(j *Job, s *none, a account) error { return nil },
	}
	r := runBot(t, b, Options{}, nil, nil)
	if r.err == nil || !strings.Contains(r.err.Error(), "missing.csv") || len(r.ends()) != 0 {
		t.Fatalf("err %v, ends %v", r.err, r.ends())
	}
}

type variant struct {
	Attempts int    `json:"attempts"`
	Proxies  string `json:"proxies"`
	Key      string `json:"key"`
}

func TestRunSettingsOverConfig(t *testing.T) {
	var seen []string
	b := Bot[variant, int]{
		Settings: func(f *form.Form, s *variant) {
			f.Int("attempts", &s.Attempts).Default(2).Min(1)
			f.File("proxies", &s.Proxies).Tag(form.TagProxies).Default("proxies.txt")
			f.String("key", &s.Key).Secret()
		},
		Jobs: func(r *Run, s *variant) iter.Seq[int] { return Times(s.Attempts, func(i int) int { return i }) },
		Job: func(j *Job, s *variant, _ int) error {
			seen = append(seen, s.Proxies+" "+s.Key)
			return nil
		},
	}
	files := map[string]string{"proxies.txt": "a.example:1000\n", "list1.txt": "b.example:2000\n"}
	r := runBot(t, b, Options{Settings: map[string]any{"proxies": "list1.txt", "key": "k1"}, Label: "list 1"}, nil, files)
	if r.err != nil || len(seen) != 2 || seen[0] != "list1.txt k1" {
		t.Fatalf("err %v, seen %v", r.err, seen)
	}
	start := r.events[0]
	changed, _ := json.Marshal(start.Data["settings"])
	config, _ := json.Marshal(start.Data["config"])
	if start.Data["label"] != "list 1" || string(changed) != `{"proxies":"list1.txt"}` || string(config) != `{"attempts":2,"proxies":"list1.txt"}` {
		t.Fatalf("run event = %+v", start.Data)
	}
	if data, err := os.ReadFile(filepath.Join(r.dir, "settings.json")); err != nil || strings.Contains(string(data), "k1") {
		t.Fatalf("settings.json: %s, %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(r.dir, "..", "..", "data", "config.json")); err == nil {
		t.Fatal("the run's settings were saved to config.json")
	}

	r = runBot(t, b, Options{Settings: map[string]any{"nope": 1}}, nil, files)
	if r.err == nil || !strings.Contains(r.err.Error(), `"nope"`) {
		t.Fatalf("unknown setting: %v", r.err)
	}
	r = runBot(t, b, Options{Settings: map[string]any{"attempts": 0}}, nil, files)
	if r.err == nil || !strings.Contains(r.err.Error(), "this run's settings") {
		t.Fatalf("invalid setting: %v", r.err)
	}
}

func TestStepNames(t *testing.T) {
	f := &flowBot{}
	if got := stepName(f.fillEmail); got != "fillEmail" {
		t.Fatalf("method = %q", got)
	}
	if got := stepName(func() (Step, error) { return nil, nil }); got != "" {
		t.Fatalf("literal = %q", got)
	}
	if stepKey("birth_date") != stepKey("birthDate") {
		t.Fatal("step keys differ")
	}
}

func TestErrorKinds(t *testing.T) {
	nav := &browserscale.CommandError{Command: "navigate", Code: "net_error", Message: "net::ERR_TUNNEL_CONNECTION_FAILED"}
	if !proxyFault(nav) || proxyFault(&browserscale.CommandError{Command: "evaluate", Code: "threw"}) {
		t.Fatal("proxy fault")
	}
	if _, code, ok := sdkCode(errors.Join(errors.New("ctx"), &browserscale.WaitError{Code: "timeout"})); !ok || code != "timeout" {
		t.Fatalf("code = %q", code)
	}
	if transportFault(errors.New("plain")) || transportFault(nav) {
		t.Fatal("transport fault")
	}
}
