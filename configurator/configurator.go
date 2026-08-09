// Package configurator orchestrates the configuration phase of a task:
// pick a module from the registry, load any existing JSON config, let
// the user edit it through a Renderer, validate, save, and pick a run name.
//
// All UI work lives in a Renderer (see renderer.go and huh_renderer.go);
// the Configurator itself is purely flow control + JSON I/O so we can
// swap front-ends (CLI now, GUI later) without touching modules or run
// setup.
package configurator

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/browserscale/browserscale-kit/module"
)

// Configurator drives the configuration phase before a run.
//
// ConfigPath, FilesDirectory and RunsDirectory are absolute paths.
// FilesDirectory is typically the same directory that holds ConfigPath
// (./data): config.json, input files, and store.db live together.
type Configurator struct {
	// ConfigPath is the single JSON config file (e.g. data/config.json).
	ConfigPath string
	// FilesDirectory holds operator-supplied input files referenced by
	// file-kind config fields (usually the same as the config's directory).
	FilesDirectory string
	// RunsDirectory is the parent of per-run output folders (./runs).
	RunsDirectory string
	Renderer      Renderer

	// Optional non-interactive overrides. When set, the corresponding
	// step is skipped:
	//
	//   - PreselectedModuleName: module is looked up by Name().
	//   - PreselectedRunName: validated and used as the run folder name.
	//   - SkipEdit: load + validate ConfigPath, do not open the editor
	//     and do not write the file back. Requires ConfigPath to exist.
	//     When PreselectedRunName is also empty, a timestamp run name is
	//     chosen automatically.
	PreselectedModuleName string
	PreselectedRunName    string
	SkipEdit              bool
}

// Result is everything the caller needs to launch the run.
type Result struct {
	Module  module.Module
	RunName string
}

// Run drives the full configuration flow:
//
//  1. Pick the module — PreselectedModuleName, or auto when exactly one
//     module is registered, else Renderer.SelectModule.
//  2. schema.ApplyDefaults() then LoadJSON from ConfigPath when it exists.
//  3. EditConfig — skipped when SkipEdit; otherwise the renderer loops
//     until the schema validates against FilesDirectory.
//  4. SaveJSON to ConfigPath — skipped when SkipEdit.
//  5. Pick a run name — PreselectedRunName, or a timestamp when SkipEdit,
//     else Renderer.PromptRunName.
//
// On user cancellation at any interactive step, an error wrapping
// ErrCanceled is returned and no persistence occurs (except edits already
// saved in step 4 when the user cancels the run-name prompt).
func (c *Configurator) Run(modules []module.Module) (*Result, error) {
	if c.ConfigPath == "" {
		return nil, fmt.Errorf("configurator: ConfigPath is required")
	}

	selected, err := c.pickModule(modules)
	if err != nil {
		return nil, err
	}

	schema := selected.Schema()
	schema.ApplyDefaults()

	if _, statErr := os.Stat(c.ConfigPath); statErr == nil {
		if err := schema.LoadJSON(c.ConfigPath); err != nil && c.SkipEdit {
			return nil, fmt.Errorf("load config %s: %w", c.ConfigPath, err)
		}
	} else if c.SkipEdit {
		return nil, fmt.Errorf("config file not found: %s (run once interactively, or create it)", c.ConfigPath)
	}

	if c.SkipEdit {
		if errs := schema.Validate(c.FilesDirectory); len(errs) > 0 {
			return nil, fmt.Errorf("config %s is invalid: %s", c.ConfigPath, errs.Error())
		}
	} else {
		if c.Renderer == nil {
			return nil, fmt.Errorf("configurator: Renderer is nil (interactive edit required)")
		}
		if err := c.Renderer.EditConfig(schema, c.FilesDirectory); err != nil {
			return nil, fmt.Errorf("edit config: %w", err)
		}
		if err := os.MkdirAll(filepath.Dir(c.ConfigPath), 0o755); err != nil {
			return nil, fmt.Errorf("mkdir config dir: %w", err)
		}
		if err := schema.SaveJSON(c.ConfigPath); err != nil {
			return nil, fmt.Errorf("save config: %w", err)
		}
	}

	runName, err := c.pickRunName()
	if err != nil {
		return nil, err
	}

	return &Result{
		Module:  selected,
		RunName: runName,
	}, nil
}

func (c *Configurator) pickModule(modules []module.Module) (module.Module, error) {
	if c.PreselectedModuleName != "" {
		for _, m := range modules {
			if m.Name() == c.PreselectedModuleName {
				return m, nil
			}
		}
		available := make([]string, 0, len(modules))
		for _, m := range modules {
			available = append(available, m.Name())
		}
		return nil, fmt.Errorf("unknown module %q (available: %v)", c.PreselectedModuleName, available)
	}

	if len(modules) == 1 {
		return modules[0], nil
	}

	if c.Renderer == nil {
		return nil, fmt.Errorf("configurator: Renderer is nil (interactive module selection required)")
	}
	selected, err := c.Renderer.SelectModule(modules)
	if err != nil {
		return nil, fmt.Errorf("select module: %w", err)
	}
	if selected == nil {
		return nil, fmt.Errorf("no module selected")
	}
	return selected, nil
}

func (c *Configurator) pickRunName() (string, error) {
	if c.PreselectedRunName != "" {
		if err := ValidateRunName(c.PreselectedRunName); err != nil {
			return "", fmt.Errorf("invalid -run: %w", err)
		}
		return c.PreselectedRunName, nil
	}

	if c.SkipEdit {
		return time.Now().Format("2006-01-02_150405"), nil
	}

	if c.Renderer == nil {
		return "", fmt.Errorf("configurator: Renderer is nil (interactive run naming required)")
	}
	runName, err := c.Renderer.PromptRunName(c.RunsDirectory)
	if err != nil {
		return "", fmt.Errorf("prompt run name: %w", err)
	}
	return runName, nil
}
