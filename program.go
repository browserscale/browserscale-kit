package kit

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"iter"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	browserscale "github.com/browserscale/browserscale-go"
	"github.com/browserscale/browserscale-kit/configurator"
	"github.com/browserscale/browserscale-kit/form"
	"github.com/browserscale/browserscale-kit/proxy"
)

// Program is a bot with several tasks: kinds of jobs that run side by side
// in one run, each from its own source, with its own threads and ends. Signups
// in browsers next to a task that watches a mailbox, or a service that keeps
// running and takes its jobs from a queue. A job rents a browser only when it
// uses one, so jobs that talk to a mailbox or an API cost none.
//
// A [Bot] is a program with one task; use it unless the bot needs more.
type Program[S any] struct {
	// Name, Title and Settings are a Bot's.
	Name     string
	Title    string
	Settings func(f *form.Form, s *S)
	// Tasks are the program's kinds of jobs, each a Task with its own name.
	Tasks []Tasker[S]
	// Browser adjusts the config of every browser rented, e.g. its country.
	Browser func(c *browserscale.BrowserConfig, s *S)
	// Browsers is how many browsers the program's jobs have at most at once;
	// a job that wants one more waits for one to be given back. No limit
	// when nil or 0.
	Browsers func(s *S) int
	// Lease, Retries and StopAfter are a Bot's, for every task.
	Lease     time.Duration
	Retries   int
	StopAfter int
	// Finish runs once after every task is over, however the run ended,
	// e.g. for a summary of all of them; r counts the jobs of all tasks.
	Finish func(r *Run, s *S)
}

// Task is one kind of job of a Program. J is what one job works on.
type Task[S, J any] struct {
	// Name identifies the task in events, reports and -task.
	Name string
	// Jobs yields what the jobs work on, see Times, Until, Forever, Every,
	// At, Queue, Rows and Lines. One job with J's zero value when nil.
	Jobs func(r *Run, s *S) iter.Seq[J]
	// Threads is how many of the task's jobs run at once; 1 when nil.
	Threads func(s *S) int
	// Job does one job, as a Bot's Job does.
	Job func(j *Job, s *S, item J) error
	// Ends names the ways the task's jobs can end and what each means.
	Ends Ends
	// Done and Finish are a Bot's, for the task's jobs.
	Done   func(r *Run, s *S, item J, res Result)
	Finish func(r *Run, s *S)
}

// Tasker is a Task of any job type, for Program.Tasks.
type Tasker[S any] interface{ plan() taskPlan[S] }

// Runnable is what Main runs: a Bot or a Program.
type Runnable[S any] interface{ program() Program[S] }

type taskPlan[S any] struct {
	name    string
	ends    Ends
	threads func(s *S) int
	hasJob  bool
	finish  func(r *Run, s *S)
	// open starts the task's source: next gives the next job's work, stop
	// lets the source go.
	open func(r *Run, s *S) (next func() (work, bool), stop func())
}

// work is one job of a task: run does a try, done hears how it ended.
type work struct {
	run  func(j *Job) error
	done func(res Result)
}

func (t Task[S, J]) plan() taskPlan[S] {
	return taskPlan[S]{
		name: t.Name, ends: t.Ends, threads: t.Threads, hasJob: t.Job != nil, finish: t.Finish,
		open: func(r *Run, s *S) (func() (work, bool), func()) {
			items := Times[J](1, nil)
			if t.Jobs != nil {
				items = t.Jobs(r, s)
			}
			next, stop := iter.Pull(items)
			return func() (work, bool) {
				item, ok := next()
				if !ok {
					return work{}, false
				}
				w := work{run: func(j *Job) error { return t.Job(j, s, item) }}
				if t.Done != nil {
					w.done = func(res Result) { t.Done(r, s, item, res) }
				}
				return w, true
			}, stop
		},
	}
}

func (p Program[S]) program() Program[S] { return p }

// Main runs a Bot or a Program as a program: it reads the flags, asks for
// the settings unless -yes, and runs until the jobs are done or Ctrl-C.
func Main[S any](b Runnable[S]) {
	p := b.program()
	o, flags, err := parseOptions(os.Args[1:])
	if errors.Is(err, flag.ErrHelp) {
		return
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if flags.schema {
		data, err := p.schema(new(S)).JSONSchema()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Stdout.Write(append(data, '\n'))
		return
	}
	if flags.form {
		dataDir := or(o.Data, "data")
		if err := configurator.Serve(p.schema(new(S)), filepath.Join(dataDir, "config.json"), dataDir, os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if ep := os.Getenv("BROWSERSCALE_ENDPOINT"); ep != "" {
		browserscale.SetApiEndpoint(ep)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if flags.stdin {
		go func() {
			sc := bufio.NewScanner(os.Stdin)
			for sc.Scan() && strings.TrimSpace(sc.Text()) != "stop" {
			}
			stop()
		}()
	}
	if _, err := p.Start(ctx, o); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func (p Program[S]) name() string {
	if p.Name == "" {
		return "bot"
	}
	return p.Name
}

func (p Program[S]) schema(s *S) *form.Schema {
	f := form.New(p.name(), or(p.Title, p.name()), s)
	if p.Settings != nil {
		p.Settings(f, s)
	}
	return f.Build()
}

// plans are the tasks the run goes with: all of them, or the ones named.
func (p Program[S]) plans(only []string) ([]taskPlan[S], error) {
	if len(p.Tasks) == 0 {
		return nil, errors.New("kit: the program has no tasks")
	}
	var all []taskPlan[S]
	var names []string
	for _, t := range p.Tasks {
		tp := t.plan()
		if !tp.hasJob {
			return nil, fmt.Errorf("kit: the task %q has no Job", tp.name)
		}
		if len(p.Tasks) > 1 && tp.name == "" {
			return nil, errors.New("kit: every task of a program with several needs a Name")
		}
		if slices.Contains(names, tp.name) {
			return nil, fmt.Errorf("kit: two tasks are named %q", tp.name)
		}
		names = append(names, tp.name)
		all = append(all, tp)
	}
	if len(only) == 0 {
		return all, nil
	}
	var out []taskPlan[S]
	for _, name := range only {
		i := slices.Index(names, strings.TrimSpace(name))
		if i < 0 {
			return nil, fmt.Errorf("there's no task %q (tasks: %s)", name, strings.Join(names, ", "))
		}
		out = append(out, all[i])
	}
	return out, nil
}

// Start runs the program with o until its jobs are done, ctx ends or the run
// stops. It returns nil, nil when the user cancels the settings.
func (p Program[S]) Start(ctx context.Context, o Options) (*Report, error) {
	return p.start(ctx, o, defaultHooks())
}

// taskInfo is what a task's jobs carry of it.
type taskInfo struct {
	name string
	ends Ends
}

func (p Program[S]) start(ctx context.Context, o Options, h hooks) (*Report, error) {
	plans, err := p.plans(o.Tasks)
	if err != nil {
		return nil, err
	}
	s := new(S)
	schema := p.schema(s)
	dataDir, err := filepath.Abs(or(o.Data, "data"))
	if err != nil {
		return nil, err
	}
	runsDir, err := filepath.Abs(or(o.Runs, "runs"))
	if err != nil {
		return nil, err
	}
	for _, d := range []string{dataDir, runsDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	c := configurator.Configurator{
		ConfigPath:         filepath.Join(dataDir, "config.json"),
		FilesDirectory:     dataDir,
		RunsDirectory:      runsDir,
		PreselectedRunName: o.Run,
		SkipEdit:           o.Yes,
		Overrides:          o.Settings,
	}
	if !o.Yes {
		c.Renderer = configurator.NewHuhRenderer()
	}
	runName, err := c.Run(schema)
	if errors.Is(err, configurator.ErrCanceled) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	runDir := filepath.Join(runsDir, runName)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return nil, err
	}
	ev, err := openEvents(runDir, o.Events, o.Log)
	if err != nil {
		return nil, err
	}
	defer ev.close()

	core := newRun(ctx, dataDir)
	defer core.cancel()
	core.onStop = func(reason string) { ev.emit(Event{Type: "stop", Text: reason}) }
	rn := &runner{
		name: p.name(), core: core, ev: ev, saves: newSaver(runDir), opts: o, h: h,
		dataDir: dataDir, runDir: runDir, single: len(plans) == 1,
		lease:   or(p.Lease, 10*time.Minute),
		retries: limit(p.Retries, 2), stopAfter: limit(p.StopAfter, 10),
	}
	defer rn.closeStore()
	core.runDir, core.out, core.emit = runDir, rn.saves, ev.emit
	values := schema.ToMap()
	rn.key = apiKey(values)
	name, rule := proxySetting(schema, values)
	if cc, managed := proxy.Managed(name); managed {
		rn.country, rn.botCountry = cc, rule.Wants()[proxy.Country]
	} else {
		src, err := proxy.Open(dataDir, name, rule.Wants())
		if err != nil {
			return nil, fmt.Errorf("read proxies: %w", err)
		}
		if src.Skipped > 0 {
			ev.emit(Event{Type: "log", Text: fmt.Sprintf("%d lines of %s aren't proxies and are left out", src.Skipped, name)})
		}
		rn.proxies = newProxies(src)
	}
	if p.Browser != nil {
		rn.configure = func(c *browserscale.BrowserConfig) { p.Browser(c, s) }
	}
	if p.Browsers != nil {
		if n := p.Browsers(s); n > 0 {
			rn.slots = make(chan struct{}, n)
		}
	}

	threads := make([]int, len(plans))
	total := 0
	var tasks []map[string]any
	var ends []EndInfo
	for i, tp := range plans {
		n := 1
		if tp.threads != nil {
			n = tp.threads(s)
		}
		n = max(or(o.Threads, n), 1)
		threads[i], total = n, total+n
		list := tp.ends.list()
		for k := range list {
			list[k].Task = tp.name
		}
		ends = append(ends, list...)
		if tp.name != "" {
			tasks = append(tasks, map[string]any{"name": tp.name, "threads": n, "ends": list})
		}
	}

	config, changed := runSettings(schema, o.Settings)
	if data, err := json.MarshalIndent(config, "", "  "); err == nil {
		_ = os.WriteFile(filepath.Join(runDir, "settings.json"), data, 0o644)
	}
	started := map[string]any{"workers": total, "threads": total, "dir": runDir, "ends": ends, "config": config}
	if len(tasks) > 0 {
		started["tasks"] = tasks
	}
	if rn.slots != nil {
		started["browsers"] = cap(rn.slots)
	}
	if o.Label != "" {
		started["label"] = o.Label
	}
	if len(changed) > 0 {
		started["settings"] = changed
	}
	ev.emit(Event{Type: "run", Text: runName, Data: started})

	var ids atomic.Int64
	var wg sync.WaitGroup
	for i, tp := range plans {
		r := core.task(tp.name)
		info := &taskInfo{name: tp.name, ends: tp.ends}
		next, stopPull := tp.open(r, s)
		var pull sync.Mutex
		taken := 0
		take := func() (work, int, bool) {
			pull.Lock()
			defer pull.Unlock()
			if core.Stopped() || (o.Jobs > 0 && taken >= o.Jobs) {
				return work{}, 0, false
			}
			wk, ok := next()
			if !ok || core.Stopped() {
				return work{}, 0, false
			}
			taken++
			r.begin()
			return wk, int(ids.Add(1)), true
		}
		wg.Go(func() {
			defer stopPull()
			var tw sync.WaitGroup
			for w := 1; w <= threads[i]; w++ {
				tw.Go(func() {
					for {
						wk, id, ok := take()
						if !ok {
							return
						}
						rn.do(info, r, w, id, wk)
					}
				})
			}
			tw.Wait()
			if tp.finish != nil {
				rn.hook(tp.name, "Finish", func() { tp.finish(r, s) })
			}
			core.taskDone(r.t)
		})
	}
	wg.Wait()
	if p.Finish != nil {
		rn.hook("", "Finish", func() { p.Finish(core.whole(), s) })
	}

	rep := core.report(runName, runDir)
	done := map[string]any{"ends": rep.Ends}
	if len(rep.Tasks) > 0 {
		done["tasks"] = rep.Tasks
	}
	ev.emit(Event{Type: "done", Data: done})
	core.mu.Lock()
	err = core.err
	core.mu.Unlock()
	return rep, err
}

// do runs one job and its retries.
func (rn *runner) do(t *taskInfo, r *Run, worker, id int, wk work) {
	begin := time.Now()
	for try := 1; ; try++ {
		j := &Job{Ctx: rn.core.ctx, ID: id, Worker: worker, Try: try, Task: t.name, rn: rn, run: r, ends: t.ends}
		j.emit(Event{Type: "job"})
		err := j.call(func() error { return wk.run(j) })
		o := rn.judge(j, err)
		o = rn.release(j, o)
		if o.again && try <= o.limit && rn.core.ctx.Err() == nil {
			j.emit(Event{Type: "retry", Text: o.why})
			continue
		}
		if o.again {
			o = o.gaveUp(t.ends)
		}
		took := time.Since(begin)
		rn.end(j, o, try, took)
		if wk.done != nil {
			res := Result{ID: id, End: o.end, Tone: o.tone, Detail: o.detail, Err: err, Step: j.step, Tries: try, Took: took, ProxyID: j.ProxyID()}
			rn.hook(t.name, "Done", func() { wk.done(res) }, id)
		}
		return
	}
}

// hook runs a bot's Done or Finish; a panic in it is logged instead of
// ending the run.
func (rn *runner) hook(task, name string, fn func(), job ...int) {
	defer func() {
		if p := recover(); p != nil {
			e := Event{Type: "log", Task: task, Text: fmt.Sprintf("%s panicked: %v\n%s", name, p, debug.Stack())}
			if len(job) > 0 {
				e.Job = job[0]
			}
			rn.ev.emit(e)
		}
	}()
	fn()
}
