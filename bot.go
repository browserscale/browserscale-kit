package kit

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"time"

	browserscale "github.com/browserscale/browserscale-go"
	"github.com/browserscale/browserscale-kit/form"
)

// Bot describes a bot with one kind of job. S is its settings struct, J
// what one job works on, such as an account. A bot with several kinds of
// jobs side by side is a [Program].
type Bot[S, J any] struct {
	// Name identifies the bot, e.g. "outlook-register": the settings form's
	// module name and the bot's namespace in the store.
	Name string
	// Title heads the settings form; Name when empty.
	Title string
	// Settings declares the settings form on f, bound to s. It runs once per
	// run. Its f.Proxies field (or a file field tagged form.TagProxies) is
	// the run's proxies: a saved source (proxy:<id>, see package proxy), a
	// list file, a source spec (.json), or browserscale's managed proxies
	// ("managed", "managed:<country>" or empty); a
	// string field named apiKey holds the browserscale key unless
	// BROWSERSCALE_API_KEY is set.
	Settings func(f *form.Form, s *S)
	// Jobs yields what the jobs work on, see Times, Until, Forever, Every,
	// At, Queue, Rows and Lines. One job with J's zero value when nil.
	Jobs func(r *Run, s *S) iter.Seq[J]
	// Threads is how many jobs run at once; 1 when nil.
	Threads func(s *S) int
	// Workers is Threads' old name.
	//
	// Deprecated: use Threads.
	Workers func(s *S) int
	// Job does one job. It returns nil after End, or when done without one,
	// which ends as "done". A job rents a browser only when it uses one.
	// Every try of a job gets the same item: with a pointer type such as
	// *Account, what one try changes on it, such as a new password or a
	// step that's done, is there for the next try and for Done.
	Job func(j *Job, s *S, item J) error
	// Ends names the ways a job can end and what each means.
	Ends Ends
	// Done hears how each job ended, once, after its last try, with the
	// item it worked on: e.g. to keep a file of what's still to do, count,
	// or hand the outcome on. Jobs end on several threads at once, so Done
	// runs on them too. A job that never started, because the run stopped
	// first, isn't heard of.
	Done func(r *Run, s *S, item J, res Result)
	// Finish runs once when the jobs are over, however the run ended, e.g.
	// to write a summary; r.Context() is over when the run was canceled.
	Finish func(r *Run, s *S)
	// Browser adjusts the config of every browser rented, e.g. its country.
	Browser func(c *browserscale.BrowserConfig, s *S)
	// Browsers is how many browsers the jobs have at most at once, for jobs
	// that need one only sometimes; no limit when nil or 0.
	Browsers func(s *S) int
	// Lease is the longest a browser is rented for; 10 minutes when zero.
	Lease time.Duration
	// Retries is how often a job is tried again after trouble with the
	// browser or the proxy; 2 when zero, never when negative.
	Retries int
	// StopAfter stops the run once that many jobs in a row failed the same
	// way at the same step: unexpectedly, or because no proxy got through;
	// 10 when zero, never when negative.
	StopAfter int
}

func (b Bot[S, J]) program() Program[S] {
	threads := b.Threads
	if threads == nil {
		threads = b.Workers
	}
	return Program[S]{
		Name: b.Name, Title: b.Title, Settings: b.Settings,
		Tasks:   []Tasker[S]{Task[S, J]{Jobs: b.Jobs, Threads: threads, Job: b.Job, Ends: b.Ends, Done: b.Done, Finish: b.Finish}},
		Browser: b.Browser, Browsers: b.Browsers,
		Lease: b.Lease, Retries: b.Retries, StopAfter: b.StopAfter,
	}
}

// Start runs the bot with o until its jobs are done, ctx ends or the run
// stops. It returns nil, nil when the user cancels the settings.
func (b Bot[S, J]) Start(ctx context.Context, o Options) (*Report, error) {
	return b.program().start(ctx, o, defaultHooks())
}

func (b Bot[S, J]) start(ctx context.Context, o Options, h hooks) (*Report, error) {
	return b.program().start(ctx, o, h)
}

// Options are the command line's, for running a bot from code.
type Options struct {
	// Data holds config.json, input files and store.db; "data" when empty.
	Data string
	// Runs holds one folder per run; "runs" when empty.
	Runs string
	// Run names this run's folder: asked for, or a timestamp with Yes.
	Run string
	// Yes runs with data/config.json as it is instead of asking.
	Yes bool
	// Settings are values over data/config.json's for this run only, by
	// field name; config.json stays as it is.
	Settings map[string]any
	// Label names the run for people, e.g. "proxies 1"; the "run" event
	// carries it.
	Label string
	// Events, when set, gets every event as a JSON line.
	Events io.Writer
	// Log gets a readable line per event; nil for none.
	Log io.Writer
	// Jobs stops each task after this many jobs; Threads overrides each
	// task's.
	Jobs, Threads int
	// Tasks runs only the program's tasks named here.
	Tasks []string
	// From starts the first flow that has this step at it; Until stops that
	// flow after this one, as "stopped", and stops the run. With Hold its
	// browser keeps running.
	From, Until string
	// Attach is the session id of a running browser for the first job,
	// reached at Grpc. It keeps running afterwards.
	Attach, Grpc string
	// Hold keeps the browser of a job that failed unexpectedly, or stopped
	// after Until, running and stops the run, to look at the page and go on
	// from there.
	Hold bool
}

type mainFlags struct{ schema, form, stdin bool }

func parseOptions(args []string) (Options, mainFlags, error) {
	var o Options
	var mf mainFlags
	fs := flag.NewFlagSet(filepath.Base(os.Args[0]), flag.ContinueOnError)
	fs.StringVar(&o.Data, "data", "data", "folder with config.json, input files and store.db")
	fs.StringVar(&o.Runs, "runs", "runs", "folder for the runs' output")
	fs.StringVar(&o.Run, "run", "", "name of this run's folder; asked for, or a timestamp with -yes")
	fs.BoolVar(&o.Yes, "yes", false, "run with data/config.json as it is, without asking")
	fs.Func("settings", `values over data/config.json's for this run only, as a JSON object, e.g. {"proxies":"list1.txt"}`, func(s string) error {
		return json.Unmarshal([]byte(s), &o.Settings)
	})
	fs.StringVar(&o.Label, "label", "", "a name for this run, e.g. \"proxies 1\"")
	fs.BoolVar(&mf.schema, "schema", false, "print the settings form as JSON Schema and exit")
	fs.BoolVar(&mf.form, "form", false, "edit data/config.json over stdin and stdout as JSON lines, for another program's settings screen, and exit when stdin closes")
	events := fs.Bool("events", false, "write every event as a JSON line to stdout; the readable log goes to stderr")
	readable := fs.Bool("log", true, "write a readable line per event")
	fs.BoolVar(&mf.stdin, "stdin", false, "stop when stdin reads the line \"stop\" or closes, for running under another program")
	fs.IntVar(&o.Jobs, "jobs", 0, "stop each task after this many jobs")
	fs.IntVar(&o.Threads, "threads", 0, "run this many jobs of each task at once instead of the setting")
	fs.IntVar(&o.Threads, "workers", 0, "the old name of -threads")
	fs.Func("task", "run only these tasks of the program, by name, comma-separated", func(s string) error {
		for _, t := range strings.Split(s, ",") {
			if t = strings.TrimSpace(t); t != "" {
				o.Tasks = append(o.Tasks, t)
			}
		}
		return nil
	})
	fs.StringVar(&o.From, "from", "", "start the first flow with this step at it")
	fs.StringVar(&o.Until, "until", "", "stop that flow after this step, and the run with it")
	fs.StringVar(&o.Attach, "attach", "", "session id of a running browser for the first job; it keeps running")
	fs.StringVar(&o.Grpc, "grpc", "", "gRPC URL of the browser given with -attach")
	fs.BoolVar(&o.Hold, "hold", false, "keep the browser of a job that failed unexpectedly, or stopped by -until, and stop the run")
	if err := fs.Parse(args); err != nil {
		return o, mf, err
	}
	if (o.Attach == "") != (o.Grpc == "") {
		return o, mf, errors.New("-attach and -grpc go together")
	}
	o.Log = os.Stdout
	if *events {
		o.Events, o.Log = os.Stdout, os.Stderr
	}
	if !*readable {
		o.Log = nil
	}
	return o, mf, nil
}

// runSettings are what the run goes with: the settings that show, and of
// them those o.Settings changed. Secret ones are left out, so the run's
// folder doesn't keep them.
func runSettings(schema *form.Schema, overrides map[string]any) (config, changed map[string]any) {
	config, changed = map[string]any{}, map[string]any{}
	for _, f := range schema.Fields {
		if f.Secret {
			continue
		}
		if _, ok := overrides[f.Name]; ok {
			changed[f.Name] = f.Get()
		}
		if f.Visible() {
			config[f.Name] = f.Get()
		}
	}
	return config, changed
}

func or[T comparable](v, fallback T) T {
	var zero T
	if v == zero {
		return fallback
	}
	return v
}

// limit reads a count where 0 means the default and below 0 none.
func limit(v, fallback int) int {
	switch {
	case v == 0:
		return fallback
	case v < 0:
		return 0
	}
	return v
}

func apiKey(values map[string]any) string {
	if k := os.Getenv("BROWSERSCALE_API_KEY"); k != "" {
		return k
	}
	for _, name := range []string{"apiKey", "cloudKey"} {
		if k, ok := values[name].(string); ok && k != "" {
			return k
		}
	}
	return ""
}

func proxySetting(schema *form.Schema, values map[string]any) (string, *form.ProxyRule) {
	for _, f := range schema.Fields {
		if f.Kind == form.KindFile && f.FileTag == form.TagProxies {
			name, _ := values[f.Name].(string)
			return name, f.Proxies
		}
	}
	return "", nil
}
