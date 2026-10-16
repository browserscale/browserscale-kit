package kit

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"runtime/debug"
	"slices"
	"strings"

	browserscale "github.com/browserscale/browserscale-go"
	"github.com/browserscale/browserscale-kit/store"
)

// Step is one step of a flow. It returns the step to go on with, nil when
// the flow is over, or what End returns to end the job.
//
// Steps are usually methods of a type that embeds *Job, so the method's
// name is the step's name in events and for -from and -until:
//
//	func (a *signup) email() (kit.Step, error) {
//		page, err := a.Page()
//		...
//		return a.password, nil
//	}
type Step func() (Step, error)

// Job is one job of a run, and each retry of it a new Job. Its browser is
// rented on first use and released when the job ends; a job that never asks
// for one runs without.
type Job struct {
	// Ctx ends when the run is canceled.
	Ctx context.Context
	// ID numbers the jobs of a run from 1, across its tasks; Worker is the
	// thread of the task running it; Try counts the tries of this job from 1.
	ID, Worker, Try int
	// Task is the name of the program's task the job belongs to; "" for a
	// Bot's.
	Task string

	rn       *runner
	run      *Run
	ends     Ends
	slot     bool
	browser  *browserscale.CloudBrowser
	page     *browserscale.Page
	attached bool
	proxy    *proxyEntry
	proxyID  string
	step     string
	ended    *ending
	watches  []watch
	until    string
}

type ending struct{ name, detail string }

type watch struct {
	cond browserscale.WaitArg
	then Step
}

// Browser is the job's browser, rented through the next proxy on first use.
// Trouble renting is the kit's to handle; an error from here only needs to
// be returned.
func (j *Job) Browser() (*browserscale.CloudBrowser, error) {
	if j.browser != nil {
		return j.browser, nil
	}
	if err := j.rn.browserFor(j); err != nil {
		return nil, err
	}
	return j.browser, nil
}

// UseProxy makes the job's browser go through the proxy with this id, for
// static proxies where an account keeps its address. Call it before the
// first Browser or Page; a resting proxy is used anyway. An empty id takes
// the next proxy in turn again.
func (j *Job) UseProxy(id string) {
	j.proxyID = id
}

// ProxyID is the id of the proxy the job's browser goes through, or "" when
// it has none, such as an entry of a plain list.
func (j *Job) ProxyID() string {
	if j.proxy == nil {
		return ""
	}
	return j.proxy.e.ID
}

// Page is the job's tab, opened on first use.
func (j *Job) Page() (*browserscale.Page, error) {
	if j.page != nil {
		return j.page, nil
	}
	b, err := j.Browser()
	if err != nil {
		return nil, err
	}
	p, err := j.rn.h.newPage(j.Ctx, b, j.attached)
	if err != nil {
		return nil, err
	}
	j.page = p
	return p, nil
}

// End ends the job with a name from the bot's Ends, such as "blocked", and
// an optional detail. A step returns what it returns:
//
//	return a.End("blocked", "riskBlock")
func (j *Job) End(name string, detail ...string) (Step, error) {
	j.ended = &ending{name: name, detail: strings.Join(detail, " ")}
	return nil, nil
}

// Ending is a step that ends the job, for Watch and for returning as the next
// step.
func (j *Job) Ending(name string, detail ...string) Step {
	return func() (Step, error) { return j.End(name, detail...) }
}

// Retry is an error that tries the job again with a fresh browser, up to the
// bot's Retries; then it ends as "gave_up".
func (j *Job) Retry(reason string) error {
	return &retryError{reason: reason}
}

// BadProxy is an error that lets the proxy rest and tries the job again with
// another one; after the bot's Retries it ends as "proxy_failed". For a site
// that turns the proxy's address away although the proxy works.
func (j *Job) BadProxy(reason string) error {
	return &retryError{reason: reason, newProxy: true}
}

// Log reports a line in the job's events.
func (j *Job) Log(format string, args ...any) {
	j.emit(Event{Type: "log", Text: fmt.Sprintf(format, args...)})
}

// Save appends record to a file in the run's folder, by default
// <name>.csv. Structs give one column per exported field, named by a csv tag
// when there is one; maps one per key. A name with another extension gets
// the record as a line of text.
func (j *Job) Save(name string, record any) error {
	file, err := j.rn.saves.save(name, record)
	if err != nil {
		return err
	}
	j.emit(Event{Type: "save", Text: file})
	return nil
}

// File resolves a file name from the settings against the data folder.
func (j *Job) File(name string) string { return j.run.File(name) }

// Run is the run as the job's task sees it: to Push to a task's Queue,
// Post a value a job Awaits, or Stop the run.
func (j *Job) Run() *Run { return j.run }

// Store is the bot's part of data/store.db, for what jobs keep across runs,
// such as accounts and their sessions.
func (j *Job) Store() (*store.Namespace, error) { return j.rn.namespace() }

// Step is the name of the step the job is in.
func (j *Job) Step() string { return j.step }

// Watch makes every Wait of this job also watch cond. When cond matches
// first, the job leaves its step and goes on with then: another step, or
// Ending for an end. For what can happen at any step, such as a block page.
//
// cond is a condition as Page.Wait takes it: a CSS or JS locator, Captcha,
// Request or Response. Keep frames out of watched conditions: a frame in one
// condition applies to the whole wait.
func (j *Job) Watch(cond browserscale.WaitArg, then Step) {
	if !isCondition(cond) {
		panic("kit: Watch takes a condition, not a wait option")
	}
	j.watches = append(j.watches, watch{cond: cond, then: then})
}

func isCondition(a browserscale.WaitArg) bool {
	switch a.(type) {
	case *browserscale.Locator, *browserscale.CaptchaCondition, *browserscale.RequestCondition, *browserscale.ResponseCondition:
		return true
	}
	return false
}

// Wait waits on the job's page until one of conds matches, like Page.Wait,
// and returns its index. The watched conditions are raced too; when one of
// them matches, Wait returns an error that Flow turns into the watch's step.
func (j *Job) Wait(args ...browserscale.WaitArg) (int, error) {
	page, err := j.Page()
	if err != nil {
		return -1, err
	}
	n := 0
	for _, a := range args {
		if isCondition(a) {
			n++
		}
	}
	all := args
	if len(j.watches) > 0 {
		all = slices.Clone(args)
		for _, w := range j.watches {
			all = append(all, w.cond)
		}
	}
	res, err := page.Wait(j.Ctx, all...)
	if err != nil {
		return -1, err
	}
	i := int(res.Index)
	if i >= n && i-n < len(j.watches) {
		w := j.watches[i-n]
		to := stepName(w.then)
		if to == "" {
			to = "end"
		}
		j.emit(Event{Type: "watch", Text: to})
		return -1, &watchHit{then: w.then, to: to}
	}
	return i, nil
}

// Flow runs steps from the first one, or from the one -from names, until a
// step returns nil or ends the job, or the one -until names is through.
// Every step the flow can reach belongs in steps, so -from and -until can
// find it. In a program with several tasks they apply to the first flow
// that has the steps they name.
func (j *Job) Flow(steps ...Step) error {
	if len(steps) == 0 {
		return errors.New("kit: Flow needs a step")
	}
	first := steps[0]
	find := func(name string) (Step, error) {
		var names []string
		for _, s := range steps {
			n := stepName(s)
			if stepKey(n) == stepKey(name) {
				return s, nil
			}
			names = append(names, n)
		}
		return nil, fmt.Errorf("kit: no step %q in this flow (steps: %s)", name, strings.Join(names, ", "))
	}
	from, until := j.rn.opts.From, j.rn.opts.Until
	claim := false
	if j.Try == 1 && (from != "" || until != "") {
		if j.rn.single {
			claim = j.ID == 1
		} else {
			has := true
			for _, name := range []string{from, until} {
				if _, err := find(name); name != "" && err != nil {
					has = false
				}
			}
			claim = has && j.rn.flowed.CompareAndSwap(false, true)
		}
	}
	if claim {
		if from := j.rn.opts.From; from != "" {
			s, err := find(from)
			if err != nil {
				return err
			}
			first = s
		}
		if until := j.rn.opts.Until; until != "" {
			if _, err := find(until); err != nil {
				return err
			}
			j.until = until
		}
	}
	return j.follow(first)
}

// pauseError stops a job after the step Options.Until names.
type pauseError struct{ step string }

func (e *pauseError) Error() string { return "stopped after " + e.step }

func (j *Job) follow(cur Step) error {
	for cur != nil {
		if err := j.Ctx.Err(); err != nil {
			return err
		}
		if name := stepName(cur); name != "" {
			j.step = name
			j.emit(Event{Type: "step"})
		}
		next, err := cur()
		if err != nil {
			var hit *watchHit
			if errors.As(err, &hit) && j.ended == nil {
				cur = hit.then
				continue
			}
			return err
		}
		if j.ended != nil {
			return nil
		}
		if j.until != "" && next != nil && stepKey(j.step) == stepKey(j.until) {
			return &pauseError{step: j.step}
		}
		cur = next
	}
	return nil
}

// call runs the job's function. A watch that matched outside a flow still
// goes on with its step.
func (j *Job) call(fn func() error) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = &panicError{value: p, stack: debug.Stack()}
		}
	}()
	err = fn()
	for err != nil && j.ended == nil {
		var hit *watchHit
		if !errors.As(err, &hit) {
			break
		}
		err = j.follow(hit.then)
	}
	return err
}

func (j *Job) emit(e Event) {
	e.Job, e.Worker, e.Try, e.Task = j.ID, j.Worker, j.Try, j.Task
	if e.Step == "" {
		e.Step = j.step
	}
	j.rn.ev.emit(e)
}

// stepName is a step function's name without its package and receiver; ""
// for function literals.
func stepName(s Step) string {
	if s == nil {
		return ""
	}
	f := runtime.FuncForPC(reflect.ValueOf(s).Pointer())
	if f == nil {
		return ""
	}
	name := strings.TrimSuffix(f.Name(), "-fm")
	if i := strings.LastIndexByte(name, '.'); i >= 0 {
		name = name[i+1:]
	}
	if strings.Trim(strings.TrimPrefix(name, "func"), "0123456789") == "" {
		return ""
	}
	return name
}

// stepKey compares step names regardless of case, _ and -, so the map's
// birth_date finds the method birthDate.
func stepKey(name string) string { return columnKey(name) }
