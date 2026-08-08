package harness

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
)

// LaunchArgs holds the flags that let a module skip parts (or all) of its
// interactive setup. Empty fields mean "ask interactively"; set fields are
// validated and applied directly.
//
//	-module NAME   module to launch (only relevant with several registered)
//	-task NAME     task / project folder name under Tasks/
//	-config PATH   config JSON to load (absolute, relative, or bare name)
//	-data DIR      working directory root (default ./data)
type LaunchArgs struct {
	Module  string
	Task    string
	Config  string
	DataDir string
}

// ParseLaunchArgs reads os.Args[1:]. -h / --help returns flag.ErrHelp,
// which the caller can treat as a clean exit.
func ParseLaunchArgs() (*LaunchArgs, error) {
	args := &LaunchArgs{}
	fs := flag.NewFlagSet(filepath.Base(os.Args[0]), flag.ContinueOnError)
	fs.StringVar(&args.Module, "module", "", "module to launch (only needed when several are registered)")
	fs.StringVar(&args.Task, "task", "", "task / project folder name; empty = prompt")
	fs.StringVar(&args.Config, "config", "", "config JSON to load; empty = use saved + interactive editor")
	fs.StringVar(&args.DataDir, "data", "./data", "working directory root (Configs/Files/Tasks/Stores live here)")
	if err := fs.Parse(os.Args[1:]); err != nil {
		return nil, err
	}
	return args, nil
}

// ResolveConfigPath turns a raw -config value into an absolute path:
//
//   - absolute path → returned unchanged
//   - relative path with a separator → resolved against the CWD
//   - bare name (no separator) → joined onto configsDirectory, with a
//     .json extension appended if missing
//
// Empty input returns "" so the caller can branch on it.
func (a *LaunchArgs) ResolveConfigPath(configsDirectory string) string {
	raw := strings.TrimSpace(a.Config)
	if raw == "" {
		return ""
	}
	if filepath.IsAbs(raw) {
		return raw
	}
	if strings.ContainsAny(raw, `/\`) {
		if abs, err := filepath.Abs(raw); err == nil {
			return abs
		}
		return raw
	}
	if !strings.EqualFold(filepath.Ext(raw), ".json") {
		raw += ".json"
	}
	return filepath.Join(configsDirectory, raw)
}
