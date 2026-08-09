// Package harness wires a set of modules into a runnable program: it parses
// launch flags, lays out data/ + runs/, drives the configurator, opens the
// persistent store, builds the module.Env, and runs the selected module
// until it finishes or the process is interrupted.
//
// Layout:
//
//	data/config.json   — single module config
//	data/*             — operator input files (CSV, proxies, …)
//	data/store.db      — persistent key/value store
//	runs/<name>/       — output for one run (registered.txt, …)
//
// Generated modules typically call:
//
//	func main() {
//	    harness.Run("my-module", []module.Module{&Module{}})
//	}
//
// harness is a convenience, not a requirement: a module author who wants
// different wiring can ignore this package and call the configurator / store
// / module directly.
package harness

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/browserscale/browserscale-kit/configurator"
	"github.com/browserscale/browserscale-kit/logger"
	"github.com/browserscale/browserscale-kit/module"
	"github.com/browserscale/browserscale-kit/store"
)

// ErrCanceled is re-exported from configurator so callers can match on
// harness.ErrCanceled without importing configurator directly.
var ErrCanceled = configurator.ErrCanceled

// Run is the batteries-included entry point. It parses flags, sets up the
// working directories, configures a module, and runs it. Returns nil on a
// clean finish (including user cancellation), or the first fatal error.
//
// appName seeds the logger prefix and the terminal title. modules is the
// set the configurator chooses from; with a single module the picker is
// skipped automatically.
func Run(appName string, modules []module.Module) error {
	l := logger.NewLogger("["+appName+"]", nil)
	setTitle(appName)

	args, err := ParseLaunchArgs()
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return fmt.Errorf("parse args: %w", err)
	}

	dataDir, err := filepath.Abs(args.DataDir)
	if err != nil {
		return fmt.Errorf("resolve data dir: %w", err)
	}
	runsDir, err := filepath.Abs(args.RunsDir)
	if err != nil {
		return fmt.Errorf("resolve runs dir: %w", err)
	}
	for _, dir := range []string{dataDir, runsDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", dir, err)
		}
	}

	configPath := filepath.Join(dataDir, "config.json")
	c := &configurator.Configurator{
		ConfigPath:            configPath,
		FilesDirectory:        dataDir,
		RunsDirectory:         runsDir,
		Renderer:              configurator.NewHuhRenderer(),
		PreselectedModuleName: args.Module,
		PreselectedRunName:    args.Run,
		SkipEdit:              args.Yes,
	}

	res, err := c.Run(modules)
	if err != nil {
		if errors.Is(err, configurator.ErrCanceled) {
			l.Println("Canceled by user.")
			return nil
		}
		return fmt.Errorf("configure: %w", err)
	}

	runDir := filepath.Join(runsDir, res.RunName)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return fmt.Errorf("mkdir run: %w", err)
	}

	kv, err := store.Open(filepath.Join(dataDir, "store.db"))
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer kv.Close()

	env := module.Env{
		FilesDirectory: dataDir,
		Output:         makeOutput(runDir, l),
		Status:         func(s string) { setTitle(appName + " | " + s) },
		Logger:         l,
		Store:          kv,
	}

	// Ctrl-C / SIGTERM cancels the context the module runs under.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
		<-sigCh
		l.Println("Interrupt received, stopping...")
		cancel()
	}()

	l.Println("Running", res.Module.Name(), "→ runs/"+res.RunName)
	if err := res.Module.Run(ctx, env); err != nil {
		l.Error("run:", err)
		return err
	}
	l.Success("Done.")
	return nil
}

// makeOutput returns an OutputFunc that streams module-produced lines to
// files under runDir. A single mutex serializes writers so concurrent
// output never interleaves within a line.
func makeOutput(runDir string, l *logger.Logger) module.OutputFunc {
	var mtx sync.Mutex
	return func(fileName, message, mode string) {
		mtx.Lock()
		defer mtx.Unlock()

		flags := os.O_WRONLY | os.O_CREATE | os.O_APPEND
		if mode == "w" || mode == "write" {
			flags = os.O_WRONLY | os.O_CREATE | os.O_TRUNC
		}
		f, err := os.OpenFile(filepath.Join(runDir, fileName), flags, 0o644)
		if err != nil {
			l.Error("open output:", err)
			return
		}
		defer f.Close()
		if _, err := fmt.Fprintln(f, message); err != nil {
			l.Error("write output:", err)
		}
	}
}

// setTitle sets the terminal window title via the OSC 0 escape sequence,
// which modern terminals on every OS understand — no per-platform code.
func setTitle(s string) {
	fmt.Fprintf(os.Stdout, "\033]0;%s\007", s)
}
