// Package store is browserscale-kit's process-wide persistent key/value store.
//
// Backend: SQLite in WAL mode via modernc.org/sqlite — a pure-Go
// SQLite (no CGO, no gcc, no extra DLLs, single binary). WAL allows
// multiple processes to share the same store file safely:
// many concurrent readers across processes, one writer at a time
// (other writers wait up to busy_timeout for the lock).
//
// Modules carve out their own slice of the store via Namespace,
// which is a virtual partition (a value in the namespace column of
// the underlying kv table). Within a namespace, keys are arbitrary
// strings and values are arbitrary byte slices.
//
// Typical lifecycle (driven by your main):
//
//	s, err := store.Open(filepath.Join(rootDir, "state.db"))
//	if err != nil { ... }
//	defer s.Close()
//
//	env := module.Env{Store: s, /* ... */}
//
// Inside a module's Run:
//
//	ns := env.Store.Namespace(m.Name())
//	sessions := store.NewJSON[SessionData](ns)
//	sessions.Update("user@example.com", func(sd SessionData, _ bool) (SessionData, error) {
//	    sd.Status = "logged_in"
//	    return sd, nil
//	})
//
// Concurrency: a single *Store may be used from many goroutines
// (and from many processes simultaneously). Reads run concurrently
// inside a process and across processes; writes are serialized by
// SQLite's reserved-lock + WAL. The Update helper wraps each
// read-modify-write in a BEGIN IMMEDIATE transaction so cross-thread
// and cross-process updates never lose writes.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// driverName is the database/sql driver registered by the import
// above. modernc registers itself as "sqlite" (note: NOT "sqlite3" —
// that name belongs to mattn/go-sqlite3, the CGO driver).
const driverName = "sqlite"

// ErrClosed is returned by any operation on a Store after Close has
// been called.
var ErrClosed = errors.New("store: closed")

// schemaSQL is run on Open. The table is namespaced internally so a
// single file can host many module's data; WITHOUT ROWID keeps the
// rows compact since (namespace, key) is the natural primary key.
const schemaSQL = `
CREATE TABLE IF NOT EXISTS kv (
    namespace TEXT NOT NULL,
    key       TEXT NOT NULL,
    value     BLOB,
    PRIMARY KEY (namespace, key)
) WITHOUT ROWID;
`

// Store is the top-level handle to the on-disk key/value database.
// Use Open to obtain one and Close to release it.
type Store struct {
	db   *sql.DB
	path string
}

// Open creates (if missing) and opens the store at path. The parent
// directory is created automatically. The database is configured for
// concurrent multi-process access:
//
//   - journal_mode=WAL   readers don't block writer (and vice versa)
//   - busy_timeout=5000  writers wait up to 5s for the lock instead
//     of erroring out with SQLITE_BUSY
//   - synchronous=NORMAL safe under WAL; one fsync per commit batch
//   - foreign_keys=ON    cheap safety net for future schema growth
//
// Multiple Open calls on the same path from the same or different
// processes are explicitly supported.
//
// Foreign-format files (e.g. a leftover database from an older build)
// cause SQLite to return "file is not a database" on the first query.
// Instead of failing, Open renames the foreign file aside with a
// timestamped suffix, sweeps any stale WAL sidecars, and retries —
// so the upgrade is transparent.
func Open(path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("store: empty path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("store: mkdir %s: %w", filepath.Dir(path), err)
	}

	s, err := openSQLite(path)
	if err == nil {
		return s, nil
	}
	if !isNotADatabaseErr(err) {
		return nil, err
	}

	// The file at path is not a SQLite database. Move it aside and try
	// again with a fresh file.
	backup, mvErr := moveAsideForeignDB(path)
	if mvErr != nil {
		return nil, fmt.Errorf("store: %s is not a database and could not be moved aside: %w (original error: %v)", path, mvErr, err)
	}
	fmt.Fprintf(os.Stderr, "[store] %s was not a SQLite database; moved to %s and created a fresh store\n", path, backup)

	return openSQLite(path)
}

// openSQLite performs the actual Open/Ping/migrate dance and returns
// either a ready *Store or a wrapped error. Separated from Open so
// the foreign-DB recovery path can call it twice without duplicating
// the DSN / pool setup.
//
// modernc.org/sqlite takes pragmas as repeated _pragma=NAME(VALUE)
// query parameters (different syntax from mattn/go-sqlite3) and
// honors _txlock=immediate to upgrade BeginTx into BEGIN IMMEDIATE
// — eliminating the SQLITE_BUSY-on-upgrade race when two writers
// (across goroutines or processes) try to escalate at the same time.
// busy_timeout then transparently waits for the lock.
func openSQLite(path string) (*Store, error) {
	dsn := "file:" + url.PathEscape(path) +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=foreign_keys(ON)" +
		"&_txlock=immediate"

	db, err := sql.Open(driverName, dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	// Bound the pool so we don't open dozens of WAL readers under
	// concurrent goroutines — SQLite is happiest with a handful of
	// connections, not hundreds.
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(4)

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: ping %s: %w", path, err)
	}
	if _, err := db.Exec(schemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: schema: %w", err)
	}
	return &Store{db: db, path: path}, nil
}

// isNotADatabaseErr reports whether err is the SQLite "file is not
// a database" code (SQLITE_NOTADB, 26). We match on the canonical
// message string so we don't have to import the driver type for
// just one check.
func isNotADatabaseErr(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "file is not a database")
}

// moveAsideForeignDB renames the file at path to
// path.bak-YYYYMMDD-HHMMSS and removes any stale SQLite WAL
// sidecars (-wal, -shm) so the next Open can create a clean DB.
// Returns the backup path on success.
func moveAsideForeignDB(path string) (string, error) {
	backup := path + ".bak-" + time.Now().Format("20060102-150405")
	if err := os.Rename(path, backup); err != nil {
		return "", err
	}
	// Sweep WAL sidecars that may have been created by our failed
	// open attempt. Best-effort; ignore errors (file might not exist).
	_ = os.Remove(path + "-wal")
	_ = os.Remove(path + "-shm")
	return backup, nil
}

// Close flushes pending writes and releases the database. Safe to
// call multiple times; subsequent calls are no-ops.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	err := s.db.Close()
	s.db = nil
	return err
}

// Path returns the absolute path of the underlying database file.
// Useful for diagnostics and tests.
func (s *Store) Path() string { return s.path }

// Namespace returns an isolated view of the store. Each namespace is
// a virtual partition inside the kv table — keys in different
// namespaces never collide.
//
// Convention: use the module name (Module.Name()) as the namespace,
// or — for modules in a shared family that want to swap state — a
// stable shared identifier.
//
// The returned *Namespace is cheap and safe to reuse for the lifetime
// of the Store; do not call Namespace per operation.
func (s *Store) Namespace(name string) *Namespace {
	return &Namespace{store: s, name: name}
}
