// Package harness wires a set of modules into a runnable program: it parses
// launch flags, lays out the working directory (Configs/Files/Tasks/Stores),
// drives the configurator to pick + configure a module, opens the persistent
// store, builds the module.Env, and runs the selected module until it
// finishes or the process is interrupted.
//
// It is the browserscale-kit equivalent of solar2-core's cli.CreateTask plus
// the example-bot main() wiring — collapsed into a single Run call so a
// generated module's main.go is just:
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
// working directory, configures a module, and runs it. Returns nil on a
// clean finish (including user cancellation), or the first fatal error.
//
// appName seeds the logger prefix, the terminal title, and the store file
// name. modules is the set the configurator chooses from; with a single
// module the picker is skipped automatically.
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

	root, err := filepath.Abs(args.DataDir)
	if err != nil {
		return fmt.Errorf("resolve data dir: %w", err)
	}
	for _, sub := range []string{"Configs", "Files", "Tasks", "Stores"} {
		if err := os.MkdirAll(filepath.Join(root, sub), 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", sub, err)
		}
	}

	configsDir := filepath.Join(root, "Configs")
	c := &configurator.Configurator{
		ConfigsDirectory:      configsDir,
		FilesDirectory:        filepath.Join(root, "Files"),
		TasksDirectory:        filepath.Join(root, "Tasks"),
		Renderer:              configurator.NewHuhRenderer(),
		PreselectedModuleName: args.Module,
		PreselectedTaskName:   args.Task,
		ConfigPathOverride:    args.ResolveConfigPath(configsDir),
	}

	res, err := c.Run(modules)
	if err != nil {
		if errors.Is(err, configurator.ErrCanceled) {
			l.Println("Canceled by user.")
			return nil
		}
		return fmt.Errorf("configure: %w", err)
	}

	projectDir := filepath.Join(root, "Tasks", res.ProjectName)
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		return fmt.Errorf("mkdir project: %w", err)
	}

	kv, err := store.Open(filepath.Join(root, "Stores", appName+".db"))
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer kv.Close()

	env := module.Env{
		FilesDirectory: filepath.Join(root, "Files"),
		Output:         makeOutput(projectDir, l),
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

	l.Println("Running", res.Module.Name(), "...")
	if err := res.Module.Run(ctx, env); err != nil {
		l.Error("run:", err)
		return err
	}
	l.Success("Done.")
	return nil
}

// makeOutput returns an OutputFunc that streams module-produced lines to
// files under projectDir. A single mutex serializes writers so concurrent
// output never interleaves within a line.
func makeOutput(projectDir string, l *logger.Logger) module.OutputFunc {
	var mtx sync.Mutex
	return func(fileName, message, mode string) {
		mtx.Lock()
		defer mtx.Unlock()

		flags := os.O_WRONLY | os.O_CREATE | os.O_APPEND
		if mode == "w" || mode == "write" {
			flags = os.O_WRONLY | os.O_CREATE | os.O_TRUNC
		}
		f, err := os.OpenFile(filepath.Join(projectDir, fileName), flags, 0o644)
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
