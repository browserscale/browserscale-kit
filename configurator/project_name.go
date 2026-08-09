package configurator

import (
	"errors"
	"regexp"
	"strings"
)

// invalidRunNameChars catches anything Windows / POSIX file systems
// reject in a folder name. Kept here (rather than in huh_renderer.go)
// so non-interactive callers (CLI args, future GUI) can validate
// against the same rules without going through the renderer.
var invalidRunNameChars = regexp.MustCompile(`[<>:"|?*\\/]`)

var reservedWindowsNames = map[string]struct{}{
	"CON": {}, "PRN": {}, "AUX": {}, "NUL": {},
	"COM1": {}, "COM2": {}, "COM3": {}, "COM4": {}, "COM5": {},
	"COM6": {}, "COM7": {}, "COM8": {}, "COM9": {},
	"LPT1": {}, "LPT2": {}, "LPT3": {}, "LPT4": {}, "LPT5": {},
	"LPT6": {}, "LPT7": {}, "LPT8": {}, "LPT9": {},
}

// ValidateRunName checks name against filesystem-safe rules. Existing
// run folders are allowed so a named run can be resumed and append
// outputs. The returned error messages are user-facing.
func ValidateRunName(name string) error {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return errors.New("run name cannot be empty")
	}
	if invalidRunNameChars.MatchString(trimmed) {
		return errors.New(`name contains forbidden characters (< > : " | ? * \ /)`)
	}
	if _, ok := reservedWindowsNames[strings.ToUpper(trimmed)]; ok {
		return errors.New("name is a reserved Windows name")
	}
	return nil
}

// ValidateProjectName is a deprecated alias for ValidateRunName that
// ignores tasksDirectory. Prefer ValidateRunName.
func ValidateProjectName(tasksDirectory, name string) error {
	_ = tasksDirectory
	return ValidateRunName(name)
}
