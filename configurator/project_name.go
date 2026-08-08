package configurator

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// invalidProjectNameChars catches anything Windows / POSIX file systems
// reject in a folder name. Kept here (rather than in huh_renderer.go)
// so non-interactive callers (CLI args, future GUI) can validate
// against the same rules without going through the renderer.
var invalidProjectNameChars = regexp.MustCompile(`[<>:"|?*\\/]`)

var reservedWindowsNames = map[string]struct{}{
	"CON": {}, "PRN": {}, "AUX": {}, "NUL": {},
	"COM1": {}, "COM2": {}, "COM3": {}, "COM4": {}, "COM5": {},
	"COM6": {}, "COM7": {}, "COM8": {}, "COM9": {},
	"LPT1": {}, "LPT2": {}, "LPT3": {}, "LPT4": {}, "LPT5": {},
	"LPT6": {}, "LPT7": {}, "LPT8": {}, "LPT9": {},
}

// ValidateProjectName checks name against the same rules the huh
// renderer enforces inline: non-empty, no forbidden characters, not a
// reserved Windows device name, and not colliding with an existing
// folder under tasksDirectory. The returned error messages are
// user-facing.
func ValidateProjectName(tasksDirectory, name string) error {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return errors.New("project name cannot be empty")
	}
	if invalidProjectNameChars.MatchString(trimmed) {
		return errors.New(`name contains forbidden characters (< > : " | ? * \ /)`)
	}
	if _, ok := reservedWindowsNames[strings.ToUpper(trimmed)]; ok {
		return errors.New("name is a reserved Windows name")
	}
	if _, err := os.Stat(filepath.Join(tasksDirectory, trimmed)); err == nil {
		return fmt.Errorf("a project named %q already exists", trimmed)
	}
	return nil
}
