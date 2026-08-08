// Package logger is the canonical logger for browserscale-kit.
//
// A Logger writes timestamped, optionally colored lines to stdout, prefixed
// with a configurable scope (e.g. "[App]", "[Module]", "[Thread-3]").
// Loggers can also tee their output to a file by calling SetLogPath.
//
// Loggers form a tree: NewLogger("[Child]", parent) produces a child whose
// prefix is "<parent prefix> [Child]" and which shares the parent's mutex
// (so concurrent writes across the tree never interleave) and its file
// destination.
package logger

import (
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/fatih/color"
)

// Logger is the central log handle. Methods are safe to call from multiple
// goroutines.
type Logger struct {
	Mtx         *sync.Mutex
	Prefix      string
	SaveLogPath string
}

// NewLogger returns a new Logger.
//
// If parentLogger is nil, the returned logger owns its own mutex and has
// no file destination. Otherwise the child inherits the parent's mutex
// and file path, and prepends the parent's prefix to its own (separated
// by a single space).
func NewLogger(prefix string, parentLogger *Logger) *Logger {
	if parentLogger != nil {
		return &Logger{
			Mtx:         parentLogger.Mtx,
			Prefix:      parentLogger.Prefix + " " + prefix,
			SaveLogPath: parentLogger.SaveLogPath,
		}
	}
	return &Logger{Mtx: new(sync.Mutex), Prefix: prefix}
}

// SetLogPath enables (or, if path is "", disables) teeing log lines to a
// file. The file is opened in append mode on every write so external
// truncation / rotation is tolerated.
func (l *Logger) SetLogPath(path string) {
	l.SaveLogPath = path
}

// Println writes args to stdout, prefixed and joined with spaces. The
// trailing newline is added.
func (l *Logger) Println(args ...interface{}) {
	l.write(args, func(s string) { fmt.Println(s) })
}

// Error writes a red error line to stdout.
func (l *Logger) Error(args ...interface{}) {
	l.write(args, func(s string) { color.Red(s) })
}

// Success writes a green success line to stdout.
func (l *Logger) Success(args ...interface{}) {
	l.write(args, func(s string) { color.Green(s) })
}

// write is the shared core of Println / Error / Success.
func (l *Logger) write(args []interface{}, printer func(string)) {
	l.Mtx.Lock()
	defer l.Mtx.Unlock()

	formatted := strings.TrimSuffix(fmt.Sprintln(args...), "\n")
	line := l.Prefix + " " + formatted

	printer(line)

	if l.SaveLogPath == "" {
		return
	}
	f, err := os.OpenFile(l.SaveLogPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		fmt.Println(err)
		return
	}
	defer f.Close()
	if _, err := f.WriteString(line + "\n"); err != nil {
		fmt.Println(err)
	}
}
