package kit

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Main takes a Bot and a Program alike.
var _ = func() {
	Main(Bot[none, int]{})
	Main(Program[none]{})
}

func TestProgramTasksHandOver(t *testing.T) {
	t.Setenv("BROWSERSCALE_API_KEY", "k")
	addresses := []string{"a@x", "b@x", "c@x"}
	p := Program[none]{
		Tasks: []Tasker[none]{
			Task[none, string]{
				Name:    "signup",
				Jobs:    func(r *Run, s *none) iter.Seq[string] { return slices.Values(addresses) },
				Threads: func(*none) int { return 3 },
				Job: func(j *Job, s *none, addr string) error {
					if _, err := j.Page(); err != nil {
						return err
					}
					code, err := Await[string](j, addr, 5*time.Second)
					if err != nil {
						return err
					}
					if code != "code-"+addr {
						return fmt.Errorf("got %q", code)
					}
					_, err = j.End("registered")
					return err
				},
				Ends: Ends{"registered": Good},
			},
			Task[none, int]{
				Name: "inbox",
				Jobs: func(r *Run, s *none) iter.Seq[int] { return Times[int](len(addresses), nil) },
				Job: func(j *Job, s *none, _ int) error {
					addr := addresses[j.Run().Started()-1]
					j.Run().Post(addr, "code-"+addr)
					_, err := j.End("posted")
					return err
				},
			},
		},
	}
	f := &fakeBrowserscale{}
	r := runProgram(t, p, Options{}, f, nil)
	if r.err != nil {
		t.Fatal(r.err)
	}
	if f.rented != 3 {
		t.Fatalf("rented %d browsers, want 3: the inbox's jobs use none", f.rented)
	}
	if r.rep.Tasks["signup"]["registered"] != 3 || r.rep.Tasks["inbox"]["posted"] != 3 || r.rep.Ends["registered"] != 3 {
		t.Fatalf("report %+v", r.rep)
	}
	for _, e := range r.ends() {
		if e.Task != "signup" && e.Task != "inbox" {
			t.Fatalf("end without its task: %+v", e)
		}
	}
	tasks, _ := r.events[0].Data["tasks"].([]any)
	if len(tasks) != 2 || r.events[0].Data["threads"] != float64(4) {
		t.Fatalf("run event %+v", r.events[0].Data)
	}
}

func TestAwaitRunsOut(t *testing.T) {
	b := Bot[none, int]{
		Job: func(j *Job, s *none, _ int) error {
			_, err := Await[string](j, "nobody", 20*time.Millisecond)
			if !errors.Is(err, ErrNothingPosted) {
				return fmt.Errorf("want ErrNothingPosted, got %v", err)
			}
			_, err = j.End("waited")
			return err
		},
	}
	r := runBot(t, b, Options{}, nil, nil)
	if r.err != nil || r.rep.Ends["waited"] != 1 {
		t.Fatalf("%v, %+v", r.err, r.rep)
	}
}

func TestQueueFromAnotherTask(t *testing.T) {
	var opened atomic.Int32
	p := Program[none]{
		Tasks: []Tasker[none]{
			Task[none, int]{
				Name: "find",
				Jobs: func(r *Run, s *none) iter.Seq[int] { return Times(3, func(i int) int { return i }) },
				Job: func(j *Job, s *none, i int) error {
					time.Sleep(5 * time.Millisecond)
					j.Run().Push("links", fmt.Sprintf("link-%d", i))
					return nil
				},
			},
			Task[none, string]{
				Name:    "open",
				Jobs:    func(r *Run, s *none) iter.Seq[string] { return Queue[string](r, "links") },
				Threads: func(*none) int { return 2 },
				Job: func(j *Job, s *none, link string) error {
					opened.Add(1)
					return nil
				},
			},
		},
	}
	done := make(chan result)
	go func() { done <- runProgram(t, p, Options{}, nil, nil) }()
	select {
	case r := <-done:
		if r.err != nil || opened.Load() != 3 || r.rep.Tasks["open"][EndDone] != 3 {
			t.Fatalf("opened %d, %v, %+v", opened.Load(), r.err, r.rep)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the queue didn't end once its producer was done")
	}
}

func TestQueueCrawlEndsWhenQuiet(t *testing.T) {
	var mu sync.Mutex
	var seen []int
	b := Bot[none, int]{
		Jobs:    func(r *Run, s *none) iter.Seq[int] { return Queue(r, "pages", 1) },
		Threads: func(*none) int { return 3 },
		Job: func(j *Job, s *none, page int) error {
			mu.Lock()
			seen = append(seen, page)
			mu.Unlock()
			for _, next := range []int{page * 2, page*2 + 1} {
				if next < 16 {
					j.Run().Push("pages", next)
				}
			}
			return nil
		},
	}
	done := make(chan result)
	go func() { done <- runBot(t, b, Options{}, nil, nil) }()
	select {
	case r := <-done:
		slices.Sort(seen)
		if r.err != nil || len(seen) != 15 || seen[0] != 1 || seen[14] != 15 {
			t.Fatalf("pages %v, %v", seen, r.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the crawl didn't end once nothing was left")
	}
}

func TestBrowsersLimit(t *testing.T) {
	t.Setenv("BROWSERSCALE_API_KEY", "k")
	var now, most atomic.Int32
	f := &fakeBrowserscale{}
	h := f.hooks()
	b := Bot[none, int]{
		Jobs:     func(r *Run, s *none) iter.Seq[int] { return Times[int](6, nil) },
		Threads:  func(*none) int { return 6 },
		Browsers: func(*none) int { return 2 },
		Job: func(j *Job, s *none, _ int) error {
			if j.ID%3 == 0 {
				return nil
			}
			if _, err := j.Browser(); err != nil {
				return err
			}
			n := now.Add(1)
			for {
				m := most.Load()
				if n <= m || most.CompareAndSwap(m, n) {
					break
				}
			}
			time.Sleep(10 * time.Millisecond)
			now.Add(-1)
			return nil
		},
	}
	root := t.TempDir()
	o := Options{Data: root + "/data", Runs: root + "/runs", Run: "test", Yes: true}
	rep, err := b.program().start(context.Background(), o, h)
	if err != nil {
		t.Fatal(err)
	}
	if most.Load() > 2 || f.rented != 4 || rep.Ends[EndDone] != 6 {
		t.Fatalf("at most %d browsers at once, %d rented, %+v", most.Load(), f.rented, rep)
	}
}

func TestOnlySomeTasks(t *testing.T) {
	var ran []string
	var mu sync.Mutex
	job := func(name string) func(j *Job, s *none, _ int) error {
		return func(j *Job, s *none, _ int) error {
			mu.Lock()
			ran = append(ran, name)
			mu.Unlock()
			return nil
		}
	}
	p := Program[none]{Tasks: []Tasker[none]{
		Task[none, int]{Name: "signup", Job: job("signup")},
		Task[none, int]{Name: "inbox", Job: job("inbox")},
	}}
	r := runProgram(t, p, Options{Tasks: []string{"inbox"}}, nil, nil)
	if r.err != nil || !slices.Equal(ran, []string{"inbox"}) {
		t.Fatalf("ran %v, %v", ran, r.err)
	}
	if _, err := p.plans([]string{"nope"}); err == nil {
		t.Fatal("an unknown task passed")
	}
	if _, err := (Program[none]{Tasks: []Tasker[none]{Task[none, int]{Job: job("a")}, Task[none, int]{Job: job("b")}}}).plans(nil); err == nil {
		t.Fatal("tasks without names passed")
	}
}

func TestUntilCountsItsTask(t *testing.T) {
	p := Program[none]{Tasks: []Tasker[none]{
		Task[none, int]{
			Name: "signup",
			Jobs: func(r *Run, s *none) iter.Seq[int] { return Until[int](r, 2, nil, "registered") },
			Job: func(j *Job, s *none, _ int) error {
				_, err := j.End("registered")
				return err
			},
		},
		Task[none, int]{
			Name: "other",
			Jobs: func(r *Run, s *none) iter.Seq[int] { return Times[int](5, nil) },
			Job: func(j *Job, s *none, _ int) error {
				_, err := j.End("registered")
				return err
			},
		},
	}}
	r := runProgram(t, p, Options{}, nil, nil)
	if r.err != nil || r.rep.Tasks["signup"]["registered"] != 2 || r.rep.Tasks["other"]["registered"] != 5 {
		t.Fatalf("%v, %+v", r.err, r.rep.Tasks)
	}
}

func TestFromFindsTheFlowWithTheStep(t *testing.T) {
	var started []string
	var mu sync.Mutex
	mark := func(name string) Step {
		return func() (Step, error) {
			mu.Lock()
			started = append(started, name)
			mu.Unlock()
			return nil, nil
		}
	}
	p := Program[none]{Tasks: []Tasker[none]{
		Task[none, int]{Name: "inbox", Job: func(j *Job, s *none, _ int) error {
			return j.Flow(mark("check"))
		}},
		Task[none, int]{Name: "signup", Job: func(j *Job, s *none, _ int) error {
			return j.Flow(mark("open"), stepNamed)
		}},
	}}
	r := runProgram(t, p, Options{From: "stepNamed"}, nil, nil)
	if r.err != nil || r.rep.Ends[EndDone] != 2 {
		t.Fatalf("%v, %+v", r.err, r.rep)
	}
	mu.Lock()
	defer mu.Unlock()
	if slices.Contains(started, "open") || !slices.Contains(started, "check") {
		t.Fatalf("steps %v: -from should start signup at stepNamed and leave inbox alone", started)
	}
}

func stepNamed() (Step, error) { return nil, nil }

func TestNextAt(t *testing.T) {
	loc := time.UTC
	now := time.Date(2026, 10, 8, 10, 0, 0, 0, loc)
	if got := nextAt(now, []int{9 * 60, 18*60 + 30}); !got.Equal(time.Date(2026, 10, 8, 18, 30, 0, 0, loc)) {
		t.Fatalf("got %v", got)
	}
	if got := nextAt(now, []int{9 * 60}); !got.Equal(time.Date(2026, 10, 9, 9, 0, 0, 0, loc)) {
		t.Fatalf("got %v", got)
	}
	if got := nextAt(now, []int{10 * 60}); !got.Equal(time.Date(2026, 10, 9, 10, 0, 0, 0, loc)) {
		t.Fatalf("the time it is now is tomorrow's: %v", got)
	}
}

func TestEveryStopsWithTheRun(t *testing.T) {
	b := Bot[none, int]{
		Jobs: func(r *Run, s *none) iter.Seq[int] { return Every[int](r, time.Hour, nil) },
		Job: func(j *Job, s *none, _ int) error {
			j.Run().Stop("enough")
			return nil
		},
	}
	done := make(chan result)
	go func() { done <- runBot(t, b, Options{}, nil, nil) }()
	select {
	case r := <-done:
		if r.err != nil || r.rep.Stopped != "enough" {
			t.Fatalf("%v, %+v", r.err, r.rep)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Every waited out its hour after the run stopped")
	}
}
