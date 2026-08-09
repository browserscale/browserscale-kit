package harness

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
)

// LaunchArgs holds the flags that let a module skip parts (or all) of its
// interactive setup. Empty fields mean "ask interactively" (or auto-pick
// when -yes is set).
//
//	-module NAME   module to launch (only relevant with several registered)
//	-run NAME      run folder under runs/ (empty = prompt, or timestamp with -yes)
//	-yes           skip the config editor; require data/config.json; auto-name the run if -run is empty
//	-data DIR      input + config + store (default ./data)
//	-runs DIR      per-run output root (default ./runs)
type LaunchArgs struct {
	Module  string
	Run     string
	Yes     bool
	DataDir string
	RunsDir string
}

// ParseLaunchArgs reads os.Args[1:]. -h / --help returns flag.ErrHelp,
// which the caller can treat as a clean exit.
//
// Deprecated aliases: -task is accepted as -run for one release.
func ParseLaunchArgs() (*LaunchArgs, error) {
	args := &LaunchArgs{}
	fs := flag.NewFlagSet(filepath.Base(os.Args[0]), flag.ContinueOnError)
	fs.StringVar(&args.Module, "module", "", "module to launch (only needed when several are registered)")
	fs.StringVar(&args.Run, "run", "", "run folder name under runs/; empty = prompt (or timestamp with -yes)")
	var taskAlias string
	fs.StringVar(&taskAlias, "task", "", "deprecated alias for -run")
	fs.BoolVar(&args.Yes, "yes", false, "non-interactive: use data/config.json, skip editor; auto-name run if -run empty")
	fs.StringVar(&args.DataDir, "data", "./data", "directory for config.json, input files, and store.db")
	fs.StringVar(&args.RunsDir, "runs", "./runs", "directory for per-run output folders")
	if err := fs.Parse(os.Args[1:]); err != nil {
		return nil, err
	}
	if strings.TrimSpace(args.Run) == "" {
		args.Run = strings.TrimSpace(taskAlias)
	}
	return args, nil
}
