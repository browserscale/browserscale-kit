package kit

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/browserscale/browserscale-kit/proxy"
)

const (
	proxyRest    = time.Minute
	proxyMaxRest = 30 * time.Minute
	// rotatingGrace is how many failures in a row a rotating entry takes
	// before it rests too; until then the next browser gets a new address.
	rotatingGrace = 4
)

type proxyEntry struct {
	e       proxy.Entry
	until   time.Time
	strikes int
	// rotate asks the next taker to get the mobile proxy a new address.
	rotate bool
}

func (e *proxyEntry) label() string { return e.e.Name() }

// proxies hands out proxies in turn and lets the ones that failed rest,
// longer each time they fail again.
type proxies struct {
	mu   sync.Mutex
	src  *proxy.Source
	list []*proxyEntry
	next int
	now  func() time.Time
}

func newProxies(src *proxy.Source) *proxies {
	ps := &proxies{src: src, now: time.Now}
	for _, e := range src.Entries {
		ps.list = append(ps.list, &proxyEntry{e: e})
	}
	return ps
}

// tries is how many proxies one rent may go through: a rotating entry
// counts as many, since each try gets a new address.
func (ps *proxies) tries() int {
	n := 0
	for _, e := range ps.list {
		if e.e.Rotating {
			n += proxyTries
		} else {
			n++
		}
	}
	return min(n, proxyTries)
}

// take returns the next proxy that isn't resting. When all of them rest, it
// waits for the first to be back and calls waiting once with how long.
func (ps *proxies) take(ctx context.Context, waiting func(time.Duration)) (*proxyEntry, error) {
	told := false
	for {
		ps.mu.Lock()
		now := ps.now()
		var soonest time.Time
		for i := range ps.list {
			idx := (ps.next + i) % len(ps.list)
			e := ps.list[idx]
			if !e.until.After(now) {
				ps.next = (idx + 1) % len(ps.list)
				ps.mu.Unlock()
				return e, nil
			}
			if soonest.IsZero() || e.until.Before(soonest) {
				soonest = e.until
			}
		}
		ps.mu.Unlock()
		wait := soonest.Sub(now)
		if !told && waiting != nil {
			waiting(wait)
			told = true
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, ctx.Err()
		case <-t.C:
		}
	}
}

// byID returns the entry with id, resting or not: a job that asks for one
// proxy gets that one.
func (ps *proxies) byID(id string) (*proxyEntry, error) {
	for _, e := range ps.list {
		if e.e.ID == id {
			return e, nil
		}
	}
	return nil, fmt.Errorf("the proxies have no entry with the id %q", id)
}

// rotation reports whether e should get a new address before it's used,
// and clears the request so one taker does it.
func (ps *proxies) rotation(e *proxyEntry) bool {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	r := e.rotate
	e.rotate = false
	return r
}

// rest takes e out of the rotation for a while and says what happens to it.
// A rotating entry stays in, since the next browser gets a new address,
// until it failed rotatingGrace times in a row; a mobile one is asked for a
// new address and rests while that takes.
func (ps *proxies) rest(e *proxyEntry) string {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	e.strikes++
	strikes := e.strikes
	if e.e.RotateURL != "" {
		e.rotate = true
	}
	if e.e.RotateURL != "" || e.e.Rotating {
		if strikes <= rotatingGrace {
			if e.e.RotateURL != "" {
				return "gets a new address"
			}
			return "gives the next browser a new address"
		}
		strikes -= rotatingGrace
	}
	d := min(proxyRest<<min(strikes-1, 5), proxyMaxRest)
	e.until = ps.now().Add(d)
	return "rests " + d.String()
}

// worked clears e's strikes after a job went well through it.
func (ps *proxies) worked(e *proxyEntry) {
	ps.mu.Lock()
	e.strikes = 0
	ps.mu.Unlock()
}
