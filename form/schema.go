package form

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/browserscale/browserscale-kit/input"
	"github.com/browserscale/browserscale-kit/proxy"
)

// Schema is the finalized, immutable representation of a module's
// configuration form. It owns the bound typed-struct pointer (via the
// fields' closures) so all Load/Save/Validate operations write directly
// through to the user's typed config.
//
// The Schema is created by Form.Build() and is the primary value passed
// from a module to the configurator.
type Schema struct {
	Module      string
	DisplayName string
	Fields      []*Field

	// target is the original cfg pointer passed to form.New. Currently used
	// only as a sanity handle; future versions may use reflection on it for
	// struct-tag based defaults.
	target any
}

// VisibleFields returns the ordered subset of fields whose visibility
// predicate currently evaluates to true. Hidden fields are skipped at
// render time but their values are still preserved during save so that
// toggling the controlling field back on does not lose data.
func (s *Schema) VisibleFields() []*Field {
	out := make([]*Field, 0, len(s.Fields))
	for _, f := range s.Fields {
		if f.Visible() {
			out = append(out, f)
		}
	}
	return out
}

// Field returns the field with the given name, or nil if no such field
// exists on this schema.
func (s *Schema) Field(name string) *Field {
	for _, f := range s.Fields {
		if f.Name == name {
			return f
		}
	}
	return nil
}

// ApplyDefaults writes each field's Default into its bound pointer if the
// pointer currently holds the zero value for its kind. This lets a freshly
// allocated config struct be populated with the schema's defaults without
// stomping on values the user has already set (e.g. from a loaded JSON).
func (s *Schema) ApplyDefaults() {
	for _, f := range s.Fields {
		if f.Default == nil {
			continue
		}
		if !isZeroish(f.Get()) {
			continue
		}
		_ = f.Set(f.Default)
	}
}

// LoadJSON reads the given JSON file and writes each known key into the
// corresponding field's bound pointer. Unknown keys are ignored. Missing
// keys leave the bound pointer untouched (so prior values, including
// defaults from ApplyDefaults, are preserved).
func (s *Schema) LoadJSON(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	return s.LoadMap(raw)
}

// LoadMap is the in-memory variant of LoadJSON.
func (s *Schema) LoadMap(raw map[string]any) error {
	for _, f := range s.Fields {
		v, ok := raw[f.Name]
		if !ok {
			continue
		}
		if err := f.Set(v); err != nil {
			return fmt.Errorf("field %s: %w", f.Name, err)
		}
	}
	return nil
}

// SaveJSON serializes every field's current value (regardless of
// visibility) plus a "moduleName" identifier to the given file. Hidden
// fields are intentionally still written so toggling a controlling field
// back on restores the previously entered value.
func (s *Schema) SaveJSON(path string) error {
	out := s.ToMap()
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return err
		}
	}
	return os.WriteFile(path, data, 0644)
}

// ToMap returns the current bound values as a JSON-serializable map. The
// "moduleName" key is always populated.
func (s *Schema) ToMap() map[string]any {
	out := map[string]any{
		"moduleName": s.Module,
	}
	for _, f := range s.Fields {
		out[f.Name] = f.Get()
	}
	return out
}

// ValidationError describes one failed validation for one field.
type ValidationError struct {
	Field   string
	Message string
}

func (e ValidationError) Error() string {
	return fmt.Sprintf("%s: %s", e.Field, e.Message)
}

// ValidationErrors is the aggregate result of Schema.Validate.
type ValidationErrors []ValidationError

func (e ValidationErrors) Error() string {
	if len(e) == 0 {
		return ""
	}
	if len(e) == 1 {
		return e[0].Error()
	}
	out := fmt.Sprintf("%d validation errors:", len(e))
	for _, err := range e {
		out += "\n  - " + err.Error()
	}
	return out
}

// HasErrors reports whether any errors are present.
func (e ValidationErrors) HasErrors() bool { return len(e) > 0 }

// Validate walks every visible field and runs:
//  1. Required-ness check (zero-valued required fields fail)
//  2. File-tag check via ValidateFileField, for KindFile fields
//  3. Field-level custom validator, if configured
//
// filesDirectory is prepended to file fields' bound values when looking up
// the file on disk. Pass "" to treat values as already-absolute paths.
//
// All errors are collected; Validate never aborts on the first failure.
func (s *Schema) Validate(filesDirectory string) ValidationErrors {
	var errs ValidationErrors
	for _, f := range s.VisibleFields() {
		val := f.Get()

		if f.Required && isZeroish(val) {
			errs = append(errs, ValidationError{
				Field:   f.Name,
				Message: "required",
			})
			continue
		}

		if f.Kind == KindFile {
			fname, _ := val.(string)
			if fname == "" {
				continue // optional + empty
			}
			if msg := ValidateFileField(filesDirectory, fname, f.FileTag); msg != "" {
				errs = append(errs, ValidationError{Field: f.Name, Message: msg})
				continue
			}
			if f.fileValidate != nil {
				absPath := fname
				if filesDirectory != "" && !filepath.IsAbs(fname) {
					absPath = filepath.Join(filesDirectory, fname)
				}
				if err := f.fileValidate(absPath); err != nil {
					errs = append(errs, ValidationError{Field: f.Name, Message: err.Error()})
					continue
				}
			}
		}

		if err := f.Validate(); err != nil {
			errs = append(errs, ValidationError{
				Field:   f.Name,
				Message: err.Error(),
			})
		}
	}
	return errs
}

// ValidateFileField is the standalone file-tag validator used by Validate.
// It returns an empty string on success and a human-readable error message
// on failure.
//
// The tags map directly to the input / proxy helpers:
//
//	TagRaw       -> file exists
//	TagLines     -> file exists and has at least one non-empty line
//	TagLinesPath -> same as TagLines (path is what consumers use, but
//	                content is checked the same way)
//	TagProxies   -> file exists and parses to at least one proxy
//
// Unknown tags fall back to TagRaw semantics.
func ValidateFileField(filesDirectory, fileName, tag string) string {
	full := fileName
	if filesDirectory != "" {
		full = filepath.Join(filesDirectory, fileName)
	}
	if _, err := os.Stat(full); err != nil {
		if os.IsNotExist(err) {
			return fmt.Sprintf("file not found: %s", full)
		}
		return err.Error()
	}
	switch tag {
	case TagLines, TagLinesPath:
		lines, _ := input.Lines(full)
		if countNonEmptyLines(lines) == 0 {
			return "file is empty"
		}
	case TagProxies:
		list, _ := proxy.ParseFile(full)
		if len(list) == 0 {
			return "no parseable proxies found"
		}
	}
	return ""
}

// AllowedFileTags is the (closed) list of tags Validate understands. Useful
// for renderers that want to display tag-specific UI hints (e.g. an icon
// for proxy files vs. plain text files).
var AllowedFileTags = []string{TagRaw, TagLines, TagLinesPath, TagProxies}

// IsKnownFileTag reports whether tag is one of AllowedFileTags.
func IsKnownFileTag(tag string) bool {
	return slices.Contains(AllowedFileTags, tag)
}

// ---------------- helpers ----------------

func countNonEmptyLines(lines []string) int {
	n := 0
	for _, l := range lines {
		if l != "" {
			n++
		}
	}
	return n
}

// isZeroish is a kind-agnostic emptiness test used for Required checks and
// ApplyDefaults. It treats: nil, "", false, 0, 0.0, and empty []string as
// empty.
func isZeroish(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == ""
	case bool:
		return !x
	case int:
		return x == 0
	case int32:
		return x == 0
	case int64:
		return x == 0
	case float32:
		return x == 0
	case float64:
		return x == 0
	case []string:
		return len(x) == 0
	default:
		return false
	}
}
