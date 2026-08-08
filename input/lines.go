// Package input replaces the old file_helper grab-bag with a small,
// error-honest set of helpers for the two things automation modules
// actually need from disk: reading a list file (proxies, emails,
// combos, names) and consuming it safely across worker goroutines.
//
// The three layers, smallest to largest:
//
//   - Lines / NonEmptyLines / Files — plain readers that return errors
//     instead of swallowing them (the old file_helper printed the error
//     and kept going with a nil scanner).
//   - Queue[T] — a thread-safe consumer: Next() hands each item to
//     exactly one worker, Random() samples with replacement.
//   - Combo / ParseCombo — the "user:pass" convenience on top.
package input

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// Lines reads every line of the file at path (newline-stripped) and
// returns them in order. Unlike the old ReadFileLineArray it surfaces
// open/scan errors instead of printing and continuing.
func Lines(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var lines []string
	sc := bufio.NewScanner(f)
	// Allow long lines (default bufio limit is 64KiB) — combo/cookie
	// files occasionally carry very long entries.
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return lines, nil
}

// NonEmptyLines is Lines with blank / whitespace-only lines dropped and
// each surviving line trimmed. This is what most list files want.
func NonEmptyLines(path string) ([]string, error) {
	raw, err := Lines(path)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(raw))
	for _, l := range raw {
		if t := strings.TrimSpace(l); t != "" {
			out = append(out, t)
		}
	}
	return out, nil
}

// Files returns the base names of the regular files directly under dir
// (non-recursive, subdirectories skipped). A missing directory yields an
// empty slice and a nil error so callers can treat "no files yet" and
// "no dir yet" identically.
func Files(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []string{}, nil
		}
		return nil, err
	}
	var files []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		files = append(files, filepath.Base(e.Name()))
	}
	return files, nil
}
