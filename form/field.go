package form

// Kind is the canonical type of a form field.
type Kind string

const (
	KindString     Kind = "string"
	KindInt        Kind = "int"
	KindFloat      Kind = "float"
	KindBool       Kind = "bool"
	KindSelect     Kind = "select"
	KindFile       Kind = "file"
	KindStringList Kind = "string_list"
)

// File tag constants describe what kind of content a file field is expected
// to hold. The configurator uses these tags to do a lightweight existence /
// non-empty / parseability check when saving a config. The module itself is
// always responsible for loading and using the file contents at runtime.
const (
	// TagRaw means: only check that the file exists (no content validation).
	TagRaw = ""
	// TagLines means: file must contain at least one non-empty line.
	TagLines = "lines"
	// TagLinesPath is identical to TagLines for validation purposes; the
	// distinction historically existed so the consumer would receive the
	// path rather than the parsed content. Since modules now do their own
	// loading, both tags behave identically here.
	TagLinesPath = "lines_path"
	// TagProxies means: the file must contain at least one parseable proxy
	// (host:port or host:port:user:pass per line).
	TagProxies = "proxies"
)

// SelectOption is one choice in a Select field.
type SelectOption struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// VisibleWhen is the declarative, JSON-serializable form of a visibility
// condition. It is populated when ShowWhenEq / ShowWhenIn / ShowWhenTruthy
// are used, so the resulting Schema can be exported to JSON Schema with
// conditional information preserved.
//
// Predicate-based ShowWhen does not populate VisibleWhen, since arbitrary Go
// closures cannot be serialized.
type VisibleWhen struct {
	Field string `json:"field"`
	Op    string `json:"op"` // "eq" | "ne" | "in" | "truthy"
	Value any    `json:"value,omitempty"`
}

// Field is the canonical, kind-agnostic representation of one form field.
// It is produced by the typed FieldBuilders and consumed by the renderer
// (CLI prompts) and the JSON-Schema exporter.
type Field struct {
	Name     string
	Kind     Kind
	Label    string
	Help     string
	Required bool
	Default  any

	// Options populated for Kind == KindSelect.
	Options []SelectOption

	// FileTag populated for Kind == KindFile. Empty means TagRaw.
	FileTag string

	// ptr is the typed pointer the field is bound to (*string, *int, *float64,
	// *bool, *[]string). Renderers may type-switch on this to bind the field
	// directly to UI widgets, sidestepping the get/set conversion closures.
	// The get/set closures are still used for JSON marshal/unmarshal where
	// the value type is generic `any`.
	ptr any

	// visible is the compiled predicate used at runtime. It is non-nil even
	// when the user supplied a declarative ShowWhen* (see schema.go).
	visible func() bool

	// visibleWhen is the declarative form, populated when ShowWhenEq /
	// ShowWhenIn / ShowWhenTruthy was used. Used only for JSON-Schema export.
	visibleWhen *VisibleWhen

	// validate is an optional custom validator. It receives the value that
	// would be written to the typed struct (already converted).
	validate func(any) error

	// fileValidate is an optional per-file content validator. It is
	// only meaningful for Kind == KindFile. Schema.Validate calls it
	// with the resolved absolute path (filesDirectory joined with the
	// bound filename) after the FileTag check passes, so the module
	// can do structural checks the generic tags can't express (e.g.
	// "must be a 7-column CSV").
	fileValidate func(absPath string) error

	// get returns the current value of the bound pointer, typed.
	get func() any
	// set writes a value (typically from JSON or user input) into the bound
	// pointer, performing the appropriate conversion. It returns an error
	// for unconvertible inputs.
	set func(any) error
}

// Visible reports whether this field should be shown / persisted under the
// current state of the bound config struct. Fields without an explicit
// ShowWhen are always visible.
func (f *Field) Visible() bool {
	if f.visible == nil {
		return true
	}
	return f.visible()
}

// HasPredicate reports whether the field carries any visibility predicate
// (set either via ShowWhen or compiled from a declarative ShowWhenEq /
// ShowWhenIn / ShowWhenTruthy). Fields without a predicate are always
// visible and renderers can skip wrapping them in conditional UI containers.
func (f *Field) HasPredicate() bool { return f.visible != nil }

// VisibleWhenSpec returns the declarative visibility condition, if any.
func (f *Field) VisibleWhenSpec() *VisibleWhen {
	return f.visibleWhen
}

// Ptr returns the bound pointer as an untyped any. Renderers should type-
// switch on f.Kind to recover the correct typed pointer:
//
//	switch f.Kind {
//	case form.KindString, form.KindSelect, form.KindFile: p := f.Ptr().(*string)
//	case form.KindInt:                                    p := f.Ptr().(*int)
//	case form.KindFloat:                                  p := f.Ptr().(*float64)
//	case form.KindBool:                                   p := f.Ptr().(*bool)
//	case form.KindStringList:                             p := f.Ptr().(*[]string)
//	}
//
// Binding directly to the typed pointer lets UI widgets mutate the
// underlying config struct in place, which is what makes live
// conditional visibility work as the user edits sibling fields.
func (f *Field) Ptr() any { return f.ptr }

// Get returns the current value of the bound pointer.
func (f *Field) Get() any {
	if f.get == nil {
		return nil
	}
	return f.get()
}

// Set writes a value into the bound pointer, doing the kind-appropriate
// conversion. Returns an error if the value cannot be converted.
func (f *Field) Set(v any) error {
	if f.set == nil {
		return nil
	}
	return f.set(v)
}

// Validate runs the optional custom validator against the field's current
// bound value. Returns nil if no validator was configured.
func (f *Field) Validate() error {
	if f.validate == nil {
		return nil
	}
	return f.validate(f.Get())
}

// ValidateFileContent invokes the optional ValidateFile callback
// registered on a KindFile field with the resolved absolute path of
// the picked file. Returns nil if no callback was configured or the
// field isn't a file field. Renderers should call this inline (during
// file selection) so the user gets immediate feedback instead of
// having to hit Continue first.
func (f *Field) ValidateFileContent(absPath string) error {
	if f.fileValidate == nil {
		return nil
	}
	return f.fileValidate(absPath)
}
