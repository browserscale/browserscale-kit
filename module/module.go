// Package module defines the contract every browserscale-kit module
// implements.
//
// A module is a self-contained automation unit (e.g. "example_bot",
// "price_watcher") with a typed configuration and a single entry point.
// The configurator drives the configuration UI by consuming the form
// schema returned from Schema(); the host then calls Run.
//
// # Lifecycle
//
// Unlike the older Initialize/Run/Stop split, a module has one runtime
// method:
//
//	Run(ctx context.Context, env Env) error
//
// The bound cfg has already been populated and validated by the
// configurator before Run is called, so Run can read the module's config
// directly. Cancellation is delivered through ctx (the host cancels it on
// SIGINT / shutdown); a module honours ctx in its worker loops and returns
// when it's done or when ctx is cancelled. Any teardown is a plain defer
// inside Run — there is no separate Stop.
//
// # Skeleton
//
//	type ExampleConfig struct {
//	    Threads  int    `json:"threads"`
//	    CloudKey string `json:"cloudKey"`
//	}
//
//	type Example struct {
//	    cfg    *ExampleConfig
//	    schema *form.Schema
//	}
//
//	func (e *Example) Name() string    { return "example" }
//	func (e *Example) Version() string { return "1.0.0" }
//
//	func (e *Example) Schema() *form.Schema {
//	    if e.schema != nil {
//	        return e.schema
//	    }
//	    e.cfg = &ExampleConfig{}
//	    f := form.New(e.Name(), "Example", e.cfg)
//	    f.Int("threads", &e.cfg.Threads).Label("Threads").Required().Default(1)
//	    f.String("cloudKey", &e.cfg.CloudKey).Label("Cloud Key").Required()
//	    e.schema = f.Build()
//	    return e.schema
//	}
//
//	func (e *Example) Run(ctx context.Context, env module.Env) error {
//	    l := logger.NewLogger("[Example]", env.Logger)
//	    // ... spawn workers, honour ctx, use env.Store / env.Output ...
//	    return nil
//	}
//
// Schema() may be called more than once and MUST return the same instance
// every call (or one that binds to the same cfg pointer) so that user edits
// in the UI are not silently discarded.
package module

import (
	"context"

	"github.com/browserscale/browserscale-kit/form"
	"github.com/browserscale/browserscale-kit/logger"
	"github.com/browserscale/browserscale-kit/store"
)

// OutputFunc writes a file produced by the running module to its output
// directory. mode is "write" / "w" to replace the file, or "append" / "a"
// to append.
type OutputFunc func(fileName, message, mode string)

// StatusFunc updates a human-readable status line shown by the host.
type StatusFunc func(status string)

// Env bundles the run-time services a module needs. It exists as a struct
// (rather than positional parameters) so future additions (metrics sink,
// secrets, ...) do not break every module. Any field may be the zero value
// in a minimal / test host, so nil-check optional dependencies before use.
type Env struct {
	// FilesDirectory is the absolute path to the data directory: config.json,
	// operator-supplied input files, and store.db live here. Resolve a file
	// value with filepath.Join(env.FilesDirectory, cfg.SomeFile).
	FilesDirectory string

	// Output streams output files to this run's folder under runs/.
	Output OutputFunc

	// Status updates the host status line.
	Status StatusFunc

	// Logger is the parent logger to derive module-scoped loggers from.
	Logger *logger.Logger

	// Store is the process-wide persistent key/value store (SQLite under
	// the hood). Modules carve out their own namespace for per-item state,
	// session blobs, run counters, etc.:
	//
	//	ns := env.Store.Namespace(m.Name())
	//	sessions := store.NewJSON[SessionData](ns)
	//
	// Always nil-check before use so a bare Env still runs.
	Store *store.Store
}

// Module is the contract every browserscale-kit module implements. See the
// package doc for the implementation skeleton and lifecycle.
type Module interface {
	// Name returns the canonical module identifier (e.g. "example_bot").
	// It is used as a routing key and store namespace.
	Name() string

	// Version returns a semver string for the module's implementation.
	Version() string

	// Schema returns the form schema bound to this module's typed config
	// struct. The same instance must be returned on repeated calls so the
	// configurator's edits are visible to Run.
	Schema() *form.Schema

	// Run executes the module's main work. The bound cfg is already
	// populated and validated. Run blocks until the work completes or ctx
	// is cancelled, and returns a non-nil error only on failure.
	Run(ctx context.Context, env Env) error
}
