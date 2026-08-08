package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Namespace is an isolated view of a Store. All operations are
// scoped to the namespace's slice of the underlying kv table.
// Created via Store.Namespace; do not construct directly.
type Namespace struct {
	store *Store
	name  string
}

// Name returns the namespace identifier.
func (n *Namespace) Name() string { return n.name }

// Get reads the raw bytes stored at key. The bool indicates presence:
//
//	value, ok, err := ns.Get("foo")
//	switch {
//	case err != nil:
//	    // I/O failure
//	case !ok:
//	    // missing key — distinct from "present with empty value"
//	default:
//	    // value is a fresh copy, safe to retain
//	}
func (n *Namespace) Get(key string) ([]byte, bool, error) {
	if err := n.checkOpen(); err != nil {
		return nil, false, err
	}
	if key == "" {
		return nil, false, errors.New("store: empty key")
	}

	var value []byte
	err := n.store.db.QueryRow(
		`SELECT value FROM kv WHERE namespace = ? AND key = ?`,
		n.name, key,
	).Scan(&value)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, false, nil
	case err != nil:
		return nil, false, fmt.Errorf("store: get %s/%s: %w", n.name, key, err)
	}
	if value == nil {
		// SQLite returns NULL for an empty BLOB written as nil; we
		// promised that "present with empty value" is distinct from
		// absent, so normalize to a non-nil zero-length slice.
		value = []byte{}
	}
	return value, true, nil
}

// Put writes value at key, overwriting any previous value.
//
// Storing a nil or empty slice is allowed and is distinct from "key
// absent" — Get will return (empty, true, nil) afterwards.
func (n *Namespace) Put(key string, value []byte) error {
	if err := n.checkOpen(); err != nil {
		return err
	}
	if key == "" {
		return errors.New("store: empty key")
	}
	if value == nil {
		// Normalize so the round-trip Get returns (empty, true, nil)
		// rather than (nil, true, nil); the public contract treats
		// nil and empty as the same "present" state.
		value = []byte{}
	}
	_, err := n.store.db.Exec(
		`INSERT INTO kv (namespace, key, value) VALUES (?, ?, ?)
		 ON CONFLICT(namespace, key) DO UPDATE SET value = excluded.value`,
		n.name, key, value,
	)
	if err != nil {
		return fmt.Errorf("store: put %s/%s: %w", n.name, key, err)
	}
	return nil
}

// Delete removes key from the namespace. Deleting a missing key is a
// no-op and returns nil.
func (n *Namespace) Delete(key string) error {
	if err := n.checkOpen(); err != nil {
		return err
	}
	if key == "" {
		return errors.New("store: empty key")
	}
	_, err := n.store.db.Exec(
		`DELETE FROM kv WHERE namespace = ? AND key = ?`,
		n.name, key,
	)
	if err != nil {
		return fmt.Errorf("store: delete %s/%s: %w", n.name, key, err)
	}
	return nil
}

// Update is the atomic read-modify-write helper. fn receives the
// current value (nil if the key is missing) and returns the new
// value to store. The whole operation runs inside a single
// BEGIN IMMEDIATE transaction, so concurrent Update calls — across
// goroutines AND across processes — are serialized and never lose
// writes.
//
// Return (nil, nil) from fn to delete the key, or return a non-nil
// error to abort the transaction (the value is left untouched).
//
// The byte slice passed to fn is a copy of the on-disk value and may
// be mutated freely.
func (n *Namespace) Update(key string, fn func(old []byte) ([]byte, error)) error {
	if err := n.checkOpen(); err != nil {
		return err
	}
	if key == "" {
		return errors.New("store: empty key")
	}
	if fn == nil {
		return errors.New("store: nil update fn")
	}

	ctx := context.Background()
	// BeginTx issues BEGIN IMMEDIATE (driver option _txlock=immediate),
	// so the write lock is reserved up front and a concurrent updater
	// will either wait (busy_timeout) or surface SQLITE_BUSY.
	tx, err := n.store.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var (
		old   []byte
		exist bool
	)
	row := tx.QueryRowContext(ctx,
		`SELECT value FROM kv WHERE namespace = ? AND key = ?`,
		n.name, key,
	)
	if err := row.Scan(&old); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("store: update get %s/%s: %w", n.name, key, err)
		}
	} else {
		exist = true
		if old == nil {
			old = []byte{}
		}
	}

	newVal, err := fn(old)
	if err != nil {
		return err
	}

	if newVal == nil {
		if exist {
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM kv WHERE namespace = ? AND key = ?`,
				n.name, key,
			); err != nil {
				return fmt.Errorf("store: update delete %s/%s: %w", n.name, key, err)
			}
		}
	} else {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO kv (namespace, key, value) VALUES (?, ?, ?)
			 ON CONFLICT(namespace, key) DO UPDATE SET value = excluded.value`,
			n.name, key, newVal,
		); err != nil {
			return fmt.Errorf("store: update put %s/%s: %w", n.name, key, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: update commit %s/%s: %w", n.name, key, err)
	}
	return nil
}

// Keys returns all keys in the namespace that start with prefix, in
// lexicographic (BINARY) order. Pass "" to list all keys.
//
// For very large namespaces prefer ForEach to avoid materialising the
// whole key list.
func (n *Namespace) Keys(prefix string) ([]string, error) {
	if err := n.checkOpen(); err != nil {
		return nil, err
	}

	rows, err := n.queryPrefix("key", prefix)
	if err != nil {
		return nil, fmt.Errorf("store: keys %s: %w", n.name, err)
	}
	defer rows.Close()

	var keys []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, fmt.Errorf("store: keys scan %s: %w", n.name, err)
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

// ForEach iterates key/value pairs in lexicographic order, optionally
// filtered to keys starting with prefix. The callback receives a
// copy of the value (safe to retain). Return a non-nil error from
// fn to stop iteration early; that error is propagated unchanged.
func (n *Namespace) ForEach(prefix string, fn func(key string, value []byte) error) error {
	if err := n.checkOpen(); err != nil {
		return err
	}
	if fn == nil {
		return errors.New("store: nil ForEach fn")
	}

	rows, err := n.queryPrefix("key, value", prefix)
	if err != nil {
		return fmt.Errorf("store: foreach %s: %w", n.name, err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			k string
			v []byte
		)
		if err := rows.Scan(&k, &v); err != nil {
			return fmt.Errorf("store: foreach scan %s: %w", n.name, err)
		}
		if v == nil {
			v = []byte{}
		}
		if err := fn(k, v); err != nil {
			return err
		}
	}
	return rows.Err()
}

// Count returns the number of keys in the namespace. Returns 0 for
// non-existent namespaces.
func (n *Namespace) Count() (int, error) {
	if err := n.checkOpen(); err != nil {
		return 0, err
	}
	var count int
	err := n.store.db.QueryRow(
		`SELECT COUNT(*) FROM kv WHERE namespace = ?`,
		n.name,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("store: count %s: %w", n.name, err)
	}
	return count, nil
}

// DropNamespace removes every key in the namespace. Useful for tests
// and for "wipe this module's state" operations.
func (n *Namespace) DropNamespace() error {
	if err := n.checkOpen(); err != nil {
		return err
	}
	_, err := n.store.db.Exec(`DELETE FROM kv WHERE namespace = ?`, n.name)
	if err != nil {
		return fmt.Errorf("store: drop %s: %w", n.name, err)
	}
	return nil
}

// queryPrefix builds + runs a prefix scan SELECT. cols is the
// projection (e.g. "key" or "key, value"). When prefix is empty the
// query selects every key in the namespace.
//
// The upper-bound trick (key < prefix || X'FF') works because the
// kv.key column defaults to BINARY collation: comparison is byte-wise,
// and 0xFF never appears inside a valid UTF-8 byte string except as
// part of an invalid sequence, so it is a safe sentinel above any
// realistic key.
func (n *Namespace) queryPrefix(cols, prefix string) (*sql.Rows, error) {
	if prefix == "" {
		return n.store.db.Query(
			`SELECT `+cols+` FROM kv WHERE namespace = ? ORDER BY key`,
			n.name,
		)
	}
	upper := prefix + "\xff"
	return n.store.db.Query(
		`SELECT `+cols+` FROM kv
		 WHERE namespace = ? AND key >= ? AND key < ?
		 ORDER BY key`,
		n.name, prefix, upper,
	)
}

func (n *Namespace) checkOpen() error {
	if n == nil || n.store == nil || n.store.db == nil {
		return ErrClosed
	}
	return nil
}
