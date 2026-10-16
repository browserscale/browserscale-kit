// Package configurator orchestrates the configuration phase of a run: load
// the JSON config, let the user edit it through a Renderer, validate, save,
// and pick a run name.
//
// All UI work lives in a Renderer (see renderer.go and huh_renderer.go); the
// Configurator itself is purely flow control + JSON I/O.
package configurator

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/browserscale/browserscale-kit/form"
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
	//   - PreselectedRunName: validated and used as the run folder name.
	//   - SkipEdit: load + validate ConfigPath, do not open the editor
	//     and do not write the file back. Requires ConfigPath to exist
	//     unless the schema has no required fields without a default.
	//     When PreselectedRunName is also empty, a timestamp run name is
	//     chosen automatically.
	PreselectedRunName string
	SkipEdit           bool
	// Overrides are values over ConfigPath's, by field name, for this run
	// only: they're validated with the rest but not saved.
	Overrides map[string]any
}

// Run drives the full configuration flow for schema:
//
//  1. schema.ApplyDefaults() then LoadJSON from ConfigPath when it exists.
//  2. EditConfig — skipped when SkipEdit; otherwise the renderer loops
//     until the schema validates against FilesDirectory.
//  3. SaveJSON to ConfigPath — skipped when SkipEdit.
//  4. Pick a run name — PreselectedRunName, or a timestamp when SkipEdit,
//     else Renderer.PromptRunName.
//
// On user cancellation at any interactive step, an error wrapping
// ErrCanceled is returned and no persistence occurs (except edits already
// saved in step 3 when the user cancels the run-name prompt).
func (c *Configurator) Run(schema *form.Schema) (runName string, err error) {
	if c.ConfigPath == "" {
		return "", fmt.Errorf("configurator: ConfigPath is required")
	}

	schema.ApplyDefaults()
	if _, statErr := os.Stat(c.ConfigPath); statErr == nil {
		if err := schema.LoadJSON(c.ConfigPath); err != nil && c.SkipEdit {
			return "", fmt.Errorf("load config %s: %w", c.ConfigPath, err)
		}
	}
	if err := applyOverrides(schema, c.Overrides); err != nil {
		return "", err
	}

	if c.SkipEdit {
		if errs := schema.Validate(c.FilesDirectory); len(errs) > 0 {
			if len(c.Overrides) > 0 {
				return "", fmt.Errorf("config %s with this run's settings is invalid: %s", c.ConfigPath, errs.Error())
			}
			return "", fmt.Errorf("config %s is invalid: %s", c.ConfigPath, errs.Error())
		}
	} else {
		if c.Renderer == nil {
			return "", fmt.Errorf("configurator: Renderer is nil (interactive edit required)")
		}
		c.Renderer.Greet(schema.DisplayName)
		if err := c.Renderer.EditConfig(schema, c.FilesDirectory); err != nil {
			return "", fmt.Errorf("edit config: %w", err)
		}
		if err := os.MkdirAll(filepath.Dir(c.ConfigPath), 0o755); err != nil {
			return "", fmt.Errorf("mkdir config dir: %w", err)
		}
		if err := schema.SaveJSON(c.ConfigPath); err != nil {
			return "", fmt.Errorf("save config: %w", err)
		}
	}

	return c.pickRunName()
}

func applyOverrides(schema *form.Schema, values map[string]any) error {
	names := slices.Sorted(maps.Keys(values))
	for _, name := range names {
		if schema.Field(name) == nil {
			return fmt.Errorf("the settings have no field %q", name)
		}
	}
	for _, name := range names {
		if err := schema.Field(name).Set(values[name]); err != nil {
			return fmt.Errorf("setting %s: %w", name, err)
		}
	}
	return nil
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
