package configurator

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"charm.land/huh/v2"

	"github.com/browserscale/browserscale-kit/form"
	"github.com/browserscale/browserscale-kit/input"
	"github.com/browserscale/browserscale-kit/module"
)

// HuhRenderer renders forms using charm.land/huh (v2). It binds huh
// widgets directly to the typed cfg pointers exposed by form.Field.Ptr()
// so that conditional ShowWhen predicates see live values as the user
// edits.
type HuhRenderer struct {
	// Theme overrides huh's default theme. Use nil for huh.ThemeCharm.
	Theme huh.Theme

	// Banner is printed once at the start of SelectModule. If empty, a
	// minimal default banner is used. Apps embedding browserscale-kit
	// typically override this with their own ASCII art.
	Banner string
}

// NewHuhRenderer returns a HuhRenderer wired with the default banner.
func NewHuhRenderer() *HuhRenderer { return &HuhRenderer{} }

const defaultBanner = "\033[1;33m   browserscale\033[0m\n" +
	"\033[1;33m==================================================\033[0m\n"

func (r *HuhRenderer) theme() huh.Theme {
	if r.Theme != nil {
		return r.Theme
	}
	return huh.ThemeFunc(huh.ThemeCharm)
}

// mapAbort converts huh's user-aborted error into our package-level
// ErrCanceled sentinel so callers can distinguish "user pressed Esc /
// Ctrl-C" from a real failure. Other errors are passed through.
func mapAbort(err error) error {
	if errors.Is(err, huh.ErrUserAborted) {
		return ErrCanceled
	}
	return err
}

// ---------------- SelectModule ----------------

func (r *HuhRenderer) SelectModule(modules []module.Module) (module.Module, error) {
	if len(modules) == 0 {
		return nil, errors.New("no modules available")
	}

	sorted := make([]module.Module, len(modules))
	copy(sorted, modules)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Schema().DisplayName < sorted[j].Schema().DisplayName
	})

	banner := r.Banner
	if banner == "" {
		banner = defaultBanner
	}
	fmt.Print(banner)

	options := make([]huh.Option[int], 0, len(sorted))
	for i, m := range sorted {
		s := m.Schema()
		options = append(options, huh.NewOption(
			fmt.Sprintf("%s (%s)", s.DisplayName, m.Name()),
			i,
		))
	}

	var idx int
	err := huh.NewForm(huh.NewGroup(
		huh.NewSelect[int]().
			Title("Select a module to run").
			Options(options...).
			Filtering(false).
			Value(&idx),
	)).WithTheme(r.theme()).Run()
	if err != nil {
		return nil, mapAbort(err)
	}
	return sorted[idx], nil
}

// ---------------- EditConfig ----------------

// continueKey is the sentinel value for the "Continue" menu entry. It is
// distinct from any real field name (form.Form rejects names containing
// a leading underscore).
const continueKey = "__continue__"

// EditConfig drives the menu-driven edit loop.
//
// The user sees a Select list of all currently-visible fields together
// with their current values, plus a "Continue" entry at the top.
// Picking a field opens a single-question form for that one field and
// writes the new value back to the bound cfg via the form's typed
// binding. Picking "Continue" runs schema.Validate; on success we
// return, on failure the list is shown again with the offending fields
// marked.
//
// Conditional fields (ShowWhen) appear in the list only when their
// predicate evaluates true. Because each per-field edit returns to this
// loop, toggling a controlling field reveals dependent fields on the
// very next iteration.
//
// The cursor position is preserved across iterations: after editing
// a field the menu re-opens with the cursor on that same field, not
// reset to the top.
//
// On huh.ErrUserAborted (user pressed Esc / Ctrl-C) we propagate
// ErrCanceled so the configurator can shut down cleanly without
// treating the cancel as a real failure.
func (r *HuhRenderer) EditConfig(schema *form.Schema, filesDirectory string) error {
	var lastErrors form.ValidationErrors
	lastPicked := "" // empty -> huh defaults to the first option

	for {
		picked, err := r.runMenu(schema, filesDirectory, lastErrors, lastPicked)
		if err != nil {
			return err
		}
		lastPicked = picked

		if picked == continueKey {
			errs := schema.Validate(filesDirectory)
			if !errs.HasErrors() {
				return nil
			}
			lastErrors = errs
			continue
		}

		f := schema.Field(picked)
		if f == nil {
			continue
		}
		if err := r.editField(f, filesDirectory); err != nil {
			return err
		}
		// Clear stale error indicators for whatever field we just touched;
		// the next Continue will re-run validation cleanly.
		lastErrors = removeFieldErrors(lastErrors, f.Name)
	}
}

// runMenu builds and runs the field-picker menu. The returned string is
// either continueKey or the canonical name of a field. initial seeds
// the cursor onto that option (use "" to default to the first option
// huh picks).
func (r *HuhRenderer) runMenu(schema *form.Schema, filesDirectory string, lastErrors form.ValidationErrors, initial string) (string, error) {
	errorFields := map[string]bool{}
	for _, e := range lastErrors {
		errorFields[e.Field] = true
	}

	// "Continue" sits at the top so a user with an already-valid saved
	// config can press Enter immediately to commit. Bold + arrow marker
	// keep it visually distinct from the field rows below.
	opts := make([]huh.Option[string], 0, len(schema.Fields)+1)
	opts = append(opts, huh.NewOption("\x1b[1m▶ Continue\x1b[0m", continueKey))
	for _, f := range schema.Fields {
		if !f.Visible() {
			continue
		}
		label := "  " + formatMenuEntry(f)
		if errorFields[f.Name] {
			label = "! " + formatMenuEntry(f)
		}
		opts = append(opts, huh.NewOption(label, f.Name))
	}

	title := schema.DisplayName + " — configuration"
	description := "Pick a field to edit, or Continue when done."
	if len(lastErrors) > 0 {
		description = fmt.Sprintf("%d validation error(s). Fields marked with ! need attention.", len(lastErrors))
	}

	picked := initial
	err := huh.NewForm(huh.NewGroup(
		huh.NewSelect[string]().
			Title(title).
			Description(description).
			Options(opts...).
			Filtering(false).
			Value(&picked),
	)).WithTheme(r.theme()).Run()
	if err != nil {
		return "", mapAbort(err)
	}
	return picked, nil
}

// editField presents a one-question form for the given field and writes
// the user's input back into the bound cfg via the field's typed Ptr().
func (r *HuhRenderer) editField(f *form.Field, filesDirectory string) error {
	w, err := buildWidget(f, filesDirectory)
	if err != nil {
		return err
	}
	if w == nil {
		return nil
	}
	if err := huh.NewForm(huh.NewGroup(w)).WithTheme(r.theme()).Run(); err != nil {
		return mapAbort(err)
	}
	return nil
}

// buildWidget builds the huh widget for one schema field.
//
// Each widget's Validate hook does three things in order:
//  1. format-check the raw input (e.g. parseable as int)
//  2. write the parsed value into the bound cfg pointer
//  3. run the field-level Validate (Min/Max + user-supplied) so range
//     errors surface inline as the user types, not only at Continue
//
// (3) makes the live menu marker "!" only show for problems that the
// schema-wide Validate would also catch — required + file-tag checks —
// and gives instant feedback for everything Min/Max/custom rejects.
func buildWidget(f *form.Field, filesDirectory string) (huh.Field, error) {
	title := f.Label
	if title == "" {
		title = f.Name
	}
	if f.Required {
		title = "* " + title
	}

	switch f.Kind {

	case form.KindString:
		ptr, ok := f.Ptr().(*string)
		if !ok {
			return nil, fmt.Errorf("field %q: expected *string, got %T", f.Name, f.Ptr())
		}
		w := huh.NewInput().
			Title(title).
			Value(ptr).
			Validate(func(s string) error {
				if f.Required && strings.TrimSpace(s) == "" {
					return errors.New("required")
				}
				// huh.Input.Value(ptr) has already written s into *ptr by
				// the time Validate runs, so f.Validate sees the live value.
				return f.Validate()
			})
		if f.Help != "" {
			w = w.Description(f.Help)
		}
		return w, nil

	case form.KindSelect:
		ptr, ok := f.Ptr().(*string)
		if !ok {
			return nil, fmt.Errorf("field %q: expected *string, got %T", f.Name, f.Ptr())
		}
		opts := make([]huh.Option[string], 0, len(f.Options))
		for _, o := range f.Options {
			opts = append(opts, huh.NewOption(o.Label, o.Value))
		}
		w := huh.NewSelect[string]().
			Title(title).
			Options(opts...).
			Filtering(false).
			Value(ptr).
			Validate(func(string) error { return f.Validate() })
		if f.Help != "" {
			w = w.Description(f.Help)
		}
		return w, nil

	case form.KindBool:
		ptr, ok := f.Ptr().(*bool)
		if !ok {
			return nil, fmt.Errorf("field %q: expected *bool, got %T", f.Name, f.Ptr())
		}
		w := huh.NewConfirm().
			Title(title).
			Affirmative("Yes").
			Negative("No").
			Value(ptr).
			Validate(func(bool) error { return f.Validate() })
		if f.Help != "" {
			w = w.Description(f.Help)
		}
		return w, nil

	case form.KindInt:
		ptr, ok := f.Ptr().(*int)
		if !ok {
			return nil, fmt.Errorf("field %q: expected *int, got %T", f.Name, f.Ptr())
		}
		wrapper := strconv.Itoa(*ptr)
		w := huh.NewInput().
			Title(title).
			Value(&wrapper).
			Validate(func(s string) error {
				s = strings.TrimSpace(s)
				if s == "" {
					if f.Required {
						return errors.New("required")
					}
					*ptr = 0
					return f.Validate()
				}
				n, err := strconv.Atoi(s)
				if err != nil {
					return errors.New("must be an integer")
				}
				*ptr = n
				return f.Validate()
			})
		if f.Help != "" {
			w = w.Description(f.Help)
		}
		return w, nil

	case form.KindFloat:
		ptr, ok := f.Ptr().(*float64)
		if !ok {
			return nil, fmt.Errorf("field %q: expected *float64, got %T", f.Name, f.Ptr())
		}
		wrapper := formatFloat(*ptr)
		w := huh.NewInput().
			Title(title).
			Value(&wrapper).
			Validate(func(s string) error {
				s = strings.TrimSpace(s)
				if s == "" {
					if f.Required {
						return errors.New("required")
					}
					*ptr = 0
					return f.Validate()
				}
				n, err := strconv.ParseFloat(s, 64)
				if err != nil {
					return errors.New("must be a number")
				}
				*ptr = n
				return f.Validate()
			})
		if f.Help != "" {
			w = w.Description(f.Help)
		}
		return w, nil

	case form.KindStringList:
		ptr, ok := f.Ptr().(*[]string)
		if !ok {
			return nil, fmt.Errorf("field %q: expected *[]string, got %T", f.Name, f.Ptr())
		}
		wrapper := strings.Join(*ptr, ", ")
		w := huh.NewInput().
			Title(title).
			Placeholder("comma-separated, e.g. us, gb, de").
			Value(&wrapper).
			Validate(func(s string) error {
				parts := splitCSV(s)
				if len(parts) == 0 && f.Required {
					return errors.New("required")
				}
				*ptr = parts
				return f.Validate()
			})
		if f.Help != "" {
			w = w.Description(f.Help)
		}
		return w, nil

	case form.KindFile:
		ptr, ok := f.Ptr().(*string)
		if !ok {
			return nil, fmt.Errorf("field %q: expected *string, got %T", f.Name, f.Ptr())
		}
		files, _ := input.Files(filesDirectory)
		opts := make([]huh.Option[string], 0, len(files)+1)
		// Allow keeping an empty / not-set state.
		opts = append(opts, huh.NewOption("(none)", ""))
		for _, name := range files {
			opts = append(opts, huh.NewOption(name, name))
		}
		w := huh.NewSelect[string]().
			Title(title).
			Description(fileDescription(f, filesDirectory)).
			Options(opts...).
			Filtering(false).
			Value(ptr).
			Validate(func(selected string) error {
				if selected == "" {
					if f.Required {
						return errors.New("required")
					}
					return nil
				}
				if msg := form.ValidateFileField(filesDirectory, selected, f.FileTag); msg != "" {
					return errors.New(msg)
				}
				// Per-module content check (e.g. "must be a 7-col CSV").
				// Runs inline so an obviously-wrong pick is rejected
				// before the user even hits Continue.
				absPath := selected
				if filesDirectory != "" && !filepath.IsAbs(selected) {
					absPath = filepath.Join(filesDirectory, selected)
				}
				if err := f.ValidateFileContent(absPath); err != nil {
					return err
				}
				return f.Validate()
			})
		return w, nil
	}

	return nil, fmt.Errorf("field %q: unsupported kind %q", f.Name, f.Kind)
}

// ---------------- PromptProjectName ----------------

func (r *HuhRenderer) PromptProjectName(tasksDirectory string) (string, error) {
	var name string
	err := huh.NewForm(huh.NewGroup(
		huh.NewInput().
			Title("Project name").
			Description("A folder with this name will be created under Tasks/").
			Value(&name).
			Validate(func(s string) error {
				return ValidateProjectName(tasksDirectory, s)
			}),
	)).WithTheme(r.theme()).Run()
	if err != nil {
		return "", mapAbort(err)
	}
	return strings.TrimSpace(name), nil
}

// ---------------- helpers ----------------

// formatMenuEntry renders one row of the field-picker menu, e.g.
// "* Browser Cloud API-Key: b196..." or "Regions: us, gb".
func formatMenuEntry(f *form.Field) string {
	label := f.Label
	if label == "" {
		label = f.Name
	}
	if f.Required {
		label = "* " + label
	}
	return fmt.Sprintf("%s: %s", label, formatFieldValue(f))
}

// formatFieldValue renders the current bound value of a field for
// display in the menu. Empty / zero values are shown as "(empty)" so
// users see at a glance which fields still need input. No masking is
// applied: secrets are displayed verbatim — keep your shoulders clear.
func formatFieldValue(f *form.Field) string {
	v := f.Get()
	switch f.Kind {
	case form.KindString, form.KindSelect, form.KindFile:
		s, _ := v.(string)
		if s == "" {
			return "(empty)"
		}
		return s
	case form.KindBool:
		if b, _ := v.(bool); b {
			return "[✓]"
		}
		return "[ ]"
	case form.KindInt:
		if n, _ := v.(int); n == 0 {
			return "(empty)"
		}
		return fmt.Sprint(v)
	case form.KindFloat:
		if n, _ := v.(float64); n == 0 {
			return "(empty)"
		}
		return formatFloat(v.(float64))
	case form.KindStringList:
		list, _ := v.([]string)
		if len(list) == 0 {
			return "(empty)"
		}
		return strings.Join(list, ", ")
	}
	return fmt.Sprint(v)
}

// removeFieldErrors returns a copy of errs with all entries for field
// removed. Used to clear the visual "!" marker once the user has gone
// in to edit that field.
func removeFieldErrors(errs form.ValidationErrors, field string) form.ValidationErrors {
	out := errs[:0:0]
	for _, e := range errs {
		if e.Field == field {
			continue
		}
		out = append(out, e)
	}
	return out
}

func splitCSV(s string) []string {
	out := []string{}
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func formatFloat(v float64) string {
	if v == float64(int64(v)) {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'f', -1, 64)
}

func fileDescription(f *form.Field, dir string) string {
	switch f.FileTag {
	case form.TagProxies:
		return fmt.Sprintf("Proxies file in %s", dir)
	case form.TagLines, form.TagLinesPath:
		return fmt.Sprintf("Text file (one entry per line) in %s", dir)
	default:
		return fmt.Sprintf("File in %s", dir)
	}
}
