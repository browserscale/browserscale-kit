package store

import (
	"encoding/json"
	"errors"
	"fmt"
)

// JSON is a typed convenience layer on top of Namespace that handles
// the JSON marshal / unmarshal boilerplate. Use it when your values
// are Go structs (the common case); reach for the raw Namespace API
// when you need to store opaque bytes.
//
// Construction:
//
//	type Session struct {
//	    Status  string   `json:"status"`
//	    Cookies []string `json:"cookies"`
//	}
//	sessions := store.NewJSON[Session](env.Store.Namespace("example_bot"))
//
// All JSON[T] operations are safe for concurrent use; they delegate
// to the underlying Namespace which is itself safe.
type JSON[T any] struct {
	ns *Namespace
}

// NewJSON wraps a Namespace with a typed JSON codec. The type
// parameter T must be JSON-serialisable via encoding/json. A nil
// namespace returns a JSON whose methods all fail with ErrClosed.
func NewJSON[T any](ns *Namespace) *JSON[T] { return &JSON[T]{ns: ns} }

// Get unmarshals the value at key into a T. The bool indicates
// presence. A present-but-malformed value returns (zero, true, err)
// so callers can choose to wipe or repair.
func (j *JSON[T]) Get(key string) (T, bool, error) {
	var zero T
	if j == nil || j.ns == nil {
		return zero, false, ErrClosed
	}
	raw, ok, err := j.ns.Get(key)
	if err != nil || !ok {
		return zero, ok, err
	}
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		return zero, true, fmt.Errorf("store: unmarshal %s/%s: %w", j.ns.name, key, err)
	}
	return v, true, nil
}

// Put marshals v and writes it at key, overwriting any previous
// value.
func (j *JSON[T]) Put(key string, v T) error {
	if j == nil || j.ns == nil {
		return ErrClosed
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("store: marshal %s/%s: %w", j.ns.name, key, err)
	}
	return j.ns.Put(key, raw)
}

// Update is the atomic read-modify-write helper for typed values.
// fn is called with the current value (zero T if the key is absent)
// and the present flag, and must return the new value to store.
//
// Return a non-nil error from fn to abort the transaction; the value
// is left untouched.
//
// Atomicity matches Namespace.Update: concurrent callers on the same
// key are serialised by SQLite, so updates never collide silently.
func (j *JSON[T]) Update(key string, fn func(current T, exists bool) (T, error)) error {
	if j == nil || j.ns == nil {
		return ErrClosed
	}
	if fn == nil {
		return errors.New("store: nil update fn")
	}
	return j.ns.Update(key, func(old []byte) ([]byte, error) {
		var (
			cur    T
			exists = old != nil
		)
		if exists {
			if err := json.Unmarshal(old, &cur); err != nil {
				// Treat corrupt blobs as "not present"; the fn gets a
				// zero value and the corrupt bytes are overwritten.
				cur = *new(T)
				exists = false
			}
		}
		next, err := fn(cur, exists)
		if err != nil {
			return nil, err
		}
		return json.Marshal(next)
	})
}

// Delete removes the key. No-op if absent.
func (j *JSON[T]) Delete(key string) error {
	if j == nil || j.ns == nil {
		return ErrClosed
	}
	return j.ns.Delete(key)
}

// Keys forwards to the underlying namespace for convenience.
func (j *JSON[T]) Keys(prefix string) ([]string, error) {
	if j == nil || j.ns == nil {
		return nil, ErrClosed
	}
	return j.ns.Keys(prefix)
}

// ForEach iterates over JSON-decoded values. Malformed entries are
// skipped silently (a malformed-aware variant can be added if needed).
func (j *JSON[T]) ForEach(prefix string, fn func(key string, v T) error) error {
	if j == nil || j.ns == nil {
		return ErrClosed
	}
	if fn == nil {
		return errors.New("store: nil ForEach fn")
	}
	return j.ns.ForEach(prefix, func(key string, raw []byte) error {
		var v T
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil
		}
		return fn(key, v)
	})
}
