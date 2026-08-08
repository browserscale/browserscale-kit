// Package configurator orchestrates the configuration phase of a task:
// pick a module from the registry, load any existing JSON config, let
// the user edit it through a Renderer, validate, save, and ask for a
// project name.
//
// All UI work lives in a Renderer (see renderer.go and huh_renderer.go);
// the Configurator itself is purely flow control + JSON I/O so we can
// swap front-ends (CLI now, GUI later) without touching modules or task
// setup.
package configurator

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/browserscale/browserscale-kit/module"
)

// Configurator drives the configuration phase of a task.
//
// All three directory fields are absolute paths.
type Configurator struct {
	ConfigsDirectory string
	FilesDirectory   string
	TasksDirectory   string
	Renderer         Renderer

	// Optional non-interactive overrides. When set, the corresponding
	// step is skipped:
	//
	//   - PreselectedModuleName: module is looked up in the registry by
	//     Name() instead of asking the user.
	//   - ConfigPathOverride: schema is loaded from this absolute path
	//     instead of <ConfigsDirectory>/<moduleName>.json, the editor
	//     is skipped, and the file is never written back.
	//   - PreselectedTaskName: ValidateProjectName runs on the supplied
	//     name and, if it passes, no prompt is shown.
	//
	// Each override is independent — providing only a subset still lets
	// the user complete the rest interactively.
	PreselectedModuleName string
	ConfigPathOverride    string
	PreselectedTaskName   string
}

// Result is everything the caller (cli/cmd) needs to launch the task.
type Result struct {
	Module      module.Module
	ProjectName string
}

// Run drives the full configuration flow:
//
//  1. Pick the module — either via PreselectedModuleName or
//     Renderer.SelectModule. When exactly one module is registered and
//     no name was preselected, it is chosen automatically (single-module
//     repos are the common case) and no picker is shown.
//  2. schema.ApplyDefaults() then LoadJSON from ConfigPathOverride if
//     set, else from <ConfigsDirectory>/<moduleName>.json (if it
//     exists), so the user sees their previous values as starting
//     points.
//  3. EditConfig — skipped when ConfigPathOverride is set; otherwise
//     the renderer loops until the schema validates against
//     FilesDirectory or the user cancels.
//  4. SaveJSON back to <ConfigsDirectory>/<moduleName>.json — skipped
//     when ConfigPathOverride is set so we don't trample the user's
//     externally-supplied file.
//  5. Pick a project name — either via PreselectedTaskName or
//     Renderer.PromptProjectName.
//
// On user cancellation at any interactive step, an error wrapping
// ErrCanceled is returned and no persistence occurs.
func (c *Configurator) Run(modules []module.Module) (*Result, error) {
	selected, err := c.pickModule(modules)
	if err != nil {
		return nil, err
	}

	schema := selected.Schema()
	schema.ApplyDefaults()

	defaultConfigPath := filepath.Join(c.ConfigsDirectory, selected.Name()+".json")
	loadPath := defaultConfigPath
	if c.ConfigPathOverride != "" {
		loadPath = c.ConfigPathOverride
	}

	if _, statErr := os.Stat(loadPath); statErr == nil {
		// In interactive mode a load failure is non-fatal — the user
		// just sees defaults. With an explicit override we surface the
		// error so the operator notices a busted config instead of
		// silently running with whatever defaults the schema provides.
		if err := schema.LoadJSON(loadPath); err != nil && c.ConfigPathOverride != "" {
			return nil, fmt.Errorf("load config %s: %w", loadPath, err)
		}
	} else if c.ConfigPathOverride != "" {
		return nil, fmt.Errorf("config file not found: %s", loadPath)
	}

	if c.ConfigPathOverride == "" {
		if c.Renderer == nil {
			return nil, fmt.Errorf("configurator: Renderer is nil (interactive edit required)")
		}
		if err := c.Renderer.EditConfig(schema, c.FilesDirectory); err != nil {
			return nil, fmt.Errorf("edit config: %w", err)
		}
		if err := schema.SaveJSON(defaultConfigPath); err != nil {
			return nil, fmt.Errorf("save config: %w", err)
		}
	} else {
		// Non-interactive: the supplied config must already validate.
		if errs := schema.Validate(c.FilesDirectory); len(errs) > 0 {
			return nil, fmt.Errorf("config %s is invalid: %s", loadPath, errs.Error())
		}
	}

	projectName, err := c.pickProjectName()
	if err != nil {
		return nil, err
	}

	return &Result{
		Module:      selected,
		ProjectName: projectName,
	}, nil
}

// pickModule returns the preselected module if PreselectedModuleName
// is set, auto-selects when exactly one module is registered, and
// otherwise delegates to the renderer. Errors from name lookups are
// explicit so the caller can distinguish "user canceled" from "you
// asked for a module that doesn't exist".
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

	// Single-module repos (the browserscale-kit default) skip the picker.
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

// pickProjectName uses PreselectedTaskName if set (validating it
// against the same rules as the interactive prompt) and otherwise
// asks the renderer.
func (c *Configurator) pickProjectName() (string, error) {
	if c.PreselectedTaskName != "" {
		if err := ValidateProjectName(c.TasksDirectory, c.PreselectedTaskName); err != nil {
			return "", fmt.Errorf("invalid --task: %w", err)
		}
		return c.PreselectedTaskName, nil
	}

	if c.Renderer == nil {
		return "", fmt.Errorf("configurator: Renderer is nil (interactive task naming required)")
	}
	projectName, err := c.Renderer.PromptProjectName(c.TasksDirectory)
	if err != nil {
		return "", fmt.Errorf("prompt project name: %w", err)
	}
	return projectName, nil
}
