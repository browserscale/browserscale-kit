package kit

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"sync"
	"time"
)

// Run is one run of a bot as one of its tasks sees it: the task's counts so
// far, what the tasks hand each other, and the way to stop the run early.
// Jobs sources read it, e.g. to stop once enough jobs ended well.
type Run struct {
	c *runCore
	t *taskState
}

// runCore is what the tasks of a run share.
type runCore struct {
	ctx     context.Context
	cancel  context.CancelFunc
	dataDir string
	onStop  func(reason string)
	// runDir is the run's folder, out writes its files, emit reports.
	runDir string
	out    *saver
	emit   func(Event)

	mu      sync.Mutex
	change  chan struct{}
	all     counts
	tasks   []*taskState
	stopped string
	err     error
	queues  map[string][]any
	posts   map[string][]post
}

// taskState is one task's part of a run. A task is done once its source ran
// out and its jobs ended; idle while its source waits on an empty queue.
type taskState struct {
	name string
	counts
	streak struct {
		end, step string
		n         int
	}
	done, idle bool
}

type counts struct {
	ends    map[string]int
	tones   map[Tone]int
	started int
	running int
}

type post struct {
	value any
	at    time.Time
}

// postKeep is how long a value posted for nobody waits to be taken.
const postKeep = time.Hour

func newRun(ctx context.Context, dataDir string) *runCore {
	ctx, cancel := context.WithCancel(ctx)
	return &runCore{
		ctx: ctx, cancel: cancel, dataDir: dataDir, change: make(chan struct{}),
		all: newCounts(), queues: map[string][]any{}, posts: map[string][]post{},
	}
}

func newCounts() counts { return counts{ends: map[string]int{}, tones: map[Tone]int{}} }

// task adds a task's part to the run and returns the task's view of it.
func (c *runCore) task(name string) *Run {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &taskState{name: name, counts: newCounts()}
	c.tasks = append(c.tasks, t)
	return &Run{c: c, t: t}
}

// Context ends when the run is canceled.
func (r *Run) Context() context.Context { return r.c.ctx }

// Task is the name of the task this view belongs to; "" for a Bot's.
func (r *Run) Task() string { return r.t.name }

// File resolves a file name from the settings against the data folder.
func (r *Run) File(name string) string {
	if name == "" || filepath.IsAbs(name) {
		return name
	}
	return filepath.Join(r.c.dataDir, name)
}

// Dir is the run's folder, for files a bot writes its own way, such as
// pictures or a database; Save, Rewrite and Write cover most.
func (r *Run) Dir() string { return r.c.runDir }

// Save appends record to a file in the run's folder as Job.Save does, e.g.
// from Jobs at the start of the run or from Done.
func (r *Run) Save(name string, record any, format ...Format) error {
	file, err := r.c.out.save(name, record, format...)
	if err != nil {
		return err
	}
	r.c.emit(Event{Type: "save", Task: r.t.name, Text: file})
	return nil
}

// Rewrite replaces a file in the run's folder with records, a slice, in a
// format as Save writes them, header and all; a later Save goes on after
// them. E.g. what's still to do, after every job:
//
//	r.Rewrite("remaining.csv", s.left())
//
// The file is never seen half written.
func (r *Run) Rewrite(name string, records any, format ...Format) error {
	file, err := r.c.out.rewrite(name, records, format...)
	if err != nil {
		return err
	}
	r.c.emit(Event{Type: "save", Task: r.t.name, Text: file})
	return nil
}

// Write replaces a file in the run's folder with data, such as a report or
// a page's HTML; the name may have folders. The file is never seen half
// written.
func (r *Run) Write(name string, data []byte) error {
	file, err := r.c.out.write(name, data)
	if err != nil {
		return err
	}
	r.c.emit(Event{Type: "save", Task: r.t.name, Text: file})
	return nil
}

// Count is how many of the task's jobs ended with one of these names.
func (r *Run) Count(ends ...string) int {
	r.c.mu.Lock()
	defer r.c.mu.Unlock()
	n := 0
	for _, e := range ends {
		n += r.t.ends[e]
	}
	return n
}

// Ended counts the task's jobs that ended with this tone.
func (r *Run) Ended(t Tone) int {
	r.c.mu.Lock()
	defer r.c.mu.Unlock()
	return r.t.tones[t]
}

// Started is how many of the task's jobs were handed out; Running how many
// of them are still going.
func (r *Run) Started() int {
	r.c.mu.Lock()
	defer r.c.mu.Unlock()
	return r.t.started
}

func (r *Run) Running() int {
	r.c.mu.Lock()
	defer r.c.mu.Unlock()
	return r.t.running
}

// Stop hands out no more jobs, in any task; the ones running finish.
func (r *Run) Stop(reason string) { r.c.Stop(reason) }

// Fail stops the run at once, cutting the running jobs short, and makes it
// end with err.
func (r *Run) Fail(err error) { r.c.Fail(err) }

// Stopped reports whether the run hands out no more jobs.
func (r *Run) Stopped() bool { return r.c.Stopped() }

// Push hands item to the task whose jobs come from Queue under name, e.g. a
// confirmation link a mailbox task found for a task that opens it in a
// browser. Jobs push with j.Run().Push.
func (r *Run) Push(name string, item any) {
	r.c.mu.Lock()
	r.c.queues[name] = append(r.c.queues[name], item)
	r.c.notify()
	r.c.mu.Unlock()
}

// Post leaves value under key for the job that awaits it with Await, e.g. a
// code a mailbox task found, under its recipient's address. A value nobody
// takes is dropped after an hour.
func (r *Run) Post(key string, value any) {
	now := time.Now()
	r.c.mu.Lock()
	for k, ps := range r.c.posts {
		for len(ps) > 0 && now.Sub(ps[0].at) > postKeep {
			ps = ps[1:]
		}
		if len(ps) == 0 {
			delete(r.c.posts, k)
		} else {
			r.c.posts[k] = ps
		}
	}
	r.c.posts[key] = append(r.c.posts[key], post{value: value, at: now})
	r.c.notify()
	r.c.mu.Unlock()
}

// ErrNothingPosted is the error of an Await that ran out of time.
var ErrNothingPosted = errors.New("nothing was posted")

// Await waits up to d for a value posted under key with Run.Post and takes
// it; one posted before is taken at once. With d at most 0 it waits as long
// as the run goes. Running out of time returns an error that wraps
// ErrNothingPosted.
func Await[T any](j *Job, key string, d time.Duration) (T, error) {
	var zero T
	c := j.run.c
	var timeout <-chan time.Time
	if d > 0 {
		t := time.NewTimer(d)
		defer t.Stop()
		timeout = t.C
	}
	for {
		c.mu.Lock()
		if ps := c.posts[key]; len(ps) > 0 {
			v := ps[0].value
			if len(ps) == 1 {
				delete(c.posts, key)
			} else {
				c.posts[key] = ps[1:]
			}
			c.mu.Unlock()
			t, ok := v.(T)
			if !ok {
				return zero, fmt.Errorf("kit: %q holds a %T, not a %T", key, v, zero)
			}
			return t, nil
		}
		ch := c.change
		c.mu.Unlock()
		select {
		case <-ch:
		case <-timeout:
			return zero, fmt.Errorf("%w under %q within %s", ErrNothingPosted, key, d)
		case <-j.Ctx.Done():
			return zero, j.Ctx.Err()
		}
	}
}

// pop takes the next item of a queue for task t. It reports false once the
// run stopped, or once nothing can come any more: the queue is empty, no job
// of any task runs, and every other task is done or waits on an empty queue
// too.
func (c *runCore) pop(name string, t *taskState) (any, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	defer func() { t.idle = false }()
	for {
		if c.stopped != "" || c.ctx.Err() != nil {
			return nil, false
		}
		if q := c.queues[name]; len(q) > 0 {
			item := q[0]
			if len(q) == 1 {
				delete(c.queues, name)
			} else {
				c.queues[name] = q[1:]
			}
			return item, true
		}
		if c.quiet(t) {
			return nil, false
		}
		if !t.idle {
			t.idle = true
			c.notify()
		}
		ch := c.change
		c.mu.Unlock()
		select {
		case <-ch:
		case <-c.ctx.Done():
		}
		c.mu.Lock()
	}
}

func (c *runCore) quiet(t *taskState) bool {
	for _, o := range c.tasks {
		if o.running > 0 || (o != t && !o.done && !o.idle) {
			return false
		}
	}
	return true
}

// taskDone marks a task whose source ran out and whose jobs ended.
func (c *runCore) taskDone(t *taskState) {
	c.mu.Lock()
	t.done = true
	c.notify()
	c.mu.Unlock()
}

// Stop hands out no more jobs; the ones running finish.
func (c *runCore) Stop(reason string) {
	c.mu.Lock()
	first := c.stopped == ""
	if first {
		c.stopped = reason
		c.notify()
	}
	c.mu.Unlock()
	if first && c.onStop != nil {
		c.onStop(reason)
	}
}

func (c *runCore) Fail(err error) {
	c.mu.Lock()
	if c.err == nil {
		c.err = err
	}
	c.mu.Unlock()
	c.Stop(err.Error())
	c.cancel()
}

func (c *runCore) Stopped() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stopped != "" || c.ctx.Err() != nil
}

func (c *runCore) notify() {
	close(c.change)
	c.change = make(chan struct{})
}

// changed is closed at the next change of the run.
func (c *runCore) changed() <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.change
}

// sleep waits d, and reports false when the run stopped first.
func (r *Run) sleep(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	for {
		if r.Stopped() {
			return false
		}
		select {
		case <-t.C:
			return !r.Stopped()
		case <-r.c.ctx.Done():
			return false
		case <-r.c.changed():
		}
	}
}

func (r *Run) begin() {
	r.c.mu.Lock()
	r.t.started++
	r.t.running++
	r.c.all.started++
	r.c.all.running++
	r.c.mu.Unlock()
}

// finish counts an end. It reports whether limit jobs of the task in a row
// now failed the same way: unexpectedly at the same step, or for want of a
// proxy.
func (r *Run) finish(end string, tone Tone, step string, limit int) bool {
	r.c.mu.Lock()
	defer r.c.mu.Unlock()
	for _, n := range []*counts{&r.t.counts, &r.c.all} {
		n.running--
		n.ends[end]++
		n.tones[tone]++
	}
	if end == EndProxyFailed {
		step = ""
	}
	s := &r.t.streak
	switch {
	case end != EndError && end != EndProxyFailed:
		s.end, s.step, s.n = "", "", 0
	case end == s.end && step == s.step:
		s.n++
	default:
		s.end, s.step, s.n = end, step, 1
	}
	r.c.notify()
	return s.n > 0 && limit > 0 && s.n == limit
}

// whole is the run as a whole, once its tasks are over: Count, Ended and
// Started go by the jobs of all of them.
func (c *runCore) whole() *Run {
	c.mu.Lock()
	defer c.mu.Unlock()
	return &Run{c: c, t: &taskState{counts: counts{ends: maps.Clone(c.all.ends), tones: maps.Clone(c.all.tones), started: c.all.started, running: c.all.running}, done: true}}
}

// Result is how a job ended after its last try, for a bot's Done.
type Result struct {
	// ID is the job's, as in its events.
	ID int
	// End is the name it ended with, such as "done", "blocked" or
	// "gave_up"; Tone how that reads; Detail what was said with it, such as
	// an error's text.
	End    string
	Tone   Tone
	Detail string
	// Err is what the last try returned; nil when it returned nil, e.g.
	// after End.
	Err error
	// Step is the step it was at, Tries how often it ran, Took how long all
	// of them took.
	Step  string
	Tries int
	Took  time.Duration
	// ProxyID is the id of the proxy the last try went through, "" for one
	// without, such as an entry of a plain list.
	ProxyID string
}

// Report is how a run went.
type Report struct {
	// Run is the run's name, Dir its folder.
	Run, Dir string
	// Ends counts the jobs by how they ended, Tones by the tone of that.
	Ends  map[string]int
	Tones map[Tone]int
	// Tasks counts the ends of each named task.
	Tasks map[string]map[string]int
	// Stopped says why the run stopped early; empty when it ran out of jobs.
	Stopped string
}

func (c *runCore) report(name, dir string) *Report {
	c.mu.Lock()
	defer c.mu.Unlock()
	rep := &Report{Run: name, Dir: dir, Ends: maps.Clone(c.all.ends), Tones: maps.Clone(c.all.tones), Stopped: c.stopped}
	for _, t := range c.tasks {
		if t.name != "" {
			if rep.Tasks == nil {
				rep.Tasks = map[string]map[string]int{}
			}
			rep.Tasks[t.name] = maps.Clone(t.ends)
		}
	}
	return rep
}
