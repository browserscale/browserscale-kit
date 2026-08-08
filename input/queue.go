package input

import (
	"math/rand/v2"
	"sync"
)

// Queue is a thread-safe consumer over a fixed slice of items. It is the
// canonical work source for a multi-threaded module: build one from a
// list file, then have every worker pull from it.
//
// Two consumption styles:
//
//   - Next() drains the queue in order, handing each item to exactly one
//     worker (the "process every account once" pattern). When empty it
//     returns ("", false) / (zero, false).
//   - Random() samples with replacement and never empties (the "pick a
//     random proxy / name" pattern).
//
// A zero Queue is not usable; always construct via NewQueue or the
// Load* helpers. All methods are safe for concurrent use.
type Queue[T any] struct {
	mu    sync.Mutex
	items []T
	next  int
}

// NewQueue wraps items in a Queue. The slice is copied so later mutation
// of the caller's slice does not affect the queue.
func NewQueue[T any](items []T) *Queue[T] {
	cp := make([]T, len(items))
	copy(cp, items)
	return &Queue[T]{items: cp}
}

// Next returns the next unconsumed item and advances the cursor. The
// bool is false once every item has been handed out.
func (q *Queue[T]) Next() (T, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	var zero T
	if q.next >= len(q.items) {
		return zero, false
	}
	item := q.items[q.next]
	q.next++
	return item, true
}

// Random returns a uniformly random item (with replacement). The bool is
// false only when the queue is empty. Random ignores the Next cursor, so
// it keeps working after the queue has been fully drained by Next.
func (q *Queue[T]) Random() (T, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	var zero T
	if len(q.items) == 0 {
		return zero, false
	}
	return q.items[rand.IntN(len(q.items))], true
}

// Len returns the total number of items the queue was built with.
func (q *Queue[T]) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items)
}

// Remaining returns how many items Next has not yet handed out.
func (q *Queue[T]) Remaining() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	r := len(q.items) - q.next
	if r < 0 {
		return 0
	}
	return r
}

// Reset rewinds the Next cursor so the queue can be drained again.
func (q *Queue[T]) Reset() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.next = 0
}

// LineQueue loads the non-empty, trimmed lines of a file into a string
// Queue — the common case for name/email/token lists.
func LineQueue(path string) (*Queue[string], error) {
	lines, err := NonEmptyLines(path)
	if err != nil {
		return nil, err
	}
	return NewQueue(lines), nil
}
