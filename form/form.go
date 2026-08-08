// Package form is browserscale-kit's configuration system.
//
// Each module defines a typed config struct and binds it to a Form using a
// fluent builder API. The resulting Schema is consumed by the configurator
// for CLI prompts, persisted as JSON, and (optionally) exported as a JSON
// Schema for future GUI/web frontends.
//
// Example:
//
//	type MyConfig struct {
//	    Threads     int    `json:"threads"`
//	    UseProxies  bool   `json:"useProxies"`
//	    ProxiesFile string `json:"proxiesFile"`
//	}
//
//	cfg := &MyConfig{}
//	f := form.New("my_module", "My Module", cfg)
//	f.Int("threads", &cfg.Threads).Label("Threads").Required().Default(1)
//	f.Bool("useProxies", &cfg.UseProxies).Label("Use Proxies").Default(false)
//	f.File("proxiesFile", &cfg.ProxiesFile).
//	    Label("Proxies File").
//	    Tag(form.TagProxies).
//	    ShowWhenTruthy("useProxies").
//	    Required()
//	schema := f.Build()
package form

// Form is a builder that collects fields bound to a typed config struct.
// The struct pointer is mutated by Schema.LoadJSON and by user input via
// the configurator. Visibility predicates read the same pointer, so
// conditionals are always evaluated against the current config state.
type Form struct {
	module      string
	displayName string
	target      any
	fields      []*Field
}

// New starts a new form bound to the typed config pointer cfg.
//
// module is the canonical module identifier used as the JSON filename and
// the "moduleName" key in storage. displayName is shown in the module
// picker. cfg must be a pointer to a struct; it is the same struct whose
// fields the field-starter methods (String/Int/...) take addresses of.
func New(module, displayName string, cfg any) *Form {
	return &Form{
		module:      module,
		displayName: displayName,
		target:      cfg,
	}
}

// String adds a text field bound to *ptr. name is the JSON key.
func (f *Form) String(name string, ptr *string) *StringField {
	b := newStringField(f, name, ptr)
	f.fields = append(f.fields, b.f)
	return b
}

// Int adds an integer field bound to *ptr. name is the JSON key.
func (f *Form) Int(name string, ptr *int) *IntField {
	b := newIntField(f, name, ptr)
	f.fields = append(f.fields, b.f)
	return b
}

// Float adds a float field bound to *ptr. name is the JSON key.
func (f *Form) Float(name string, ptr *float64) *FloatField {
	b := newFloatField(f, name, ptr)
	f.fields = append(f.fields, b.f)
	return b
}

// Bool adds a boolean field bound to *ptr. name is the JSON key.
func (f *Form) Bool(name string, ptr *bool) *BoolField {
	b := newBoolField(f, name, ptr)
	f.fields = append(f.fields, b.f)
	return b
}

// Select adds a single-select field bound to *ptr. Use Option() on the
// returned builder to add choices.
func (f *Form) Select(name string, ptr *string) *SelectField {
	b := newSelectField(f, name, ptr)
	f.fields = append(f.fields, b.f)
	return b
}

// File adds a file-picker field bound to *ptr (which holds the filename).
// Use Tag() to attach a validation hint (TagLines, TagProxies, ...).
func (f *Form) File(name string, ptr *string) *FileField {
	b := newFileField(f, name, ptr)
	f.fields = append(f.fields, b.f)
	return b
}

// StringList adds a multi-value string field bound to *ptr. Renderers
// typically present this as a comma-separated input or a multi-select; the
// stored form is always JSON array (["us","gb",...]).
func (f *Form) StringList(name string, ptr *[]string) *StringListField {
	b := newStringListField(f, name, ptr)
	f.fields = append(f.fields, b.f)
	return b
}

// Build finalizes the form and returns its Schema.
func (f *Form) Build() *Schema {
	return &Schema{
		Module:      f.module,
		DisplayName: f.displayName,
		Fields:      f.fields,
		target:      f.target,
	}
}

// findField returns the field with the given name, or nil if no such field
// exists yet. Used by compileVisibleWhen to resolve sibling references.
func (f *Form) findField(name string) *Field {
	for _, fld := range f.fields {
		if fld.Name == name {
			return fld
		}
	}
	return nil
}

// compileVisibleWhen turns a declarative VisibleWhen into a runtime
// predicate that reads the current value of the referenced sibling field.
//
// Because forms are built top-down, a field can only depend on a sibling
// that was added earlier. We resolve the sibling lazily at predicate-call
// time so this restriction is enforced naturally: a typo or forward
// reference results in the field being permanently hidden, which is loud
// enough to catch during development.
func (f *Form) compileVisibleWhen(vw *VisibleWhen) func() bool {
	return func() bool {
		sibling := f.findField(vw.Field)
		if sibling == nil {
			return false
		}
		current := sibling.Get()
		switch vw.Op {
		case "eq":
			return valueEquals(current, vw.Value)
		case "ne":
			return !valueEquals(current, vw.Value)
		case "in":
			list, ok := vw.Value.([]any)
			if !ok {
				return false
			}
			for _, candidate := range list {
				if valueEquals(current, candidate) {
					return true
				}
			}
			return false
		case "truthy":
			return isTruthy(current)
		default:
			return false
		}
	}
}

// valueEquals compares two values across the numeric / string / bool kinds
// the form package supports. It normalizes ints to float64 so that JSON-
// decoded numbers (always float64) compare correctly against Go int
// constants supplied in declarative conditions.
func valueEquals(a, b any) bool {
	if a == nil || b == nil {
		return a == b
	}
	if af, aok := numericToFloat(a); aok {
		if bf, bok := numericToFloat(b); bok {
			return af == bf
		}
	}
	return a == b
}

func numericToFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case int:
		return float64(x), true
	case int32:
		return float64(x), true
	case int64:
		return float64(x), true
	case float32:
		return float64(x), true
	case float64:
		return x, true
	default:
		return 0, false
	}
}

func isTruthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case int:
		return x != 0
	case int32:
		return x != 0
	case int64:
		return x != 0
	case float32:
		return x != 0
	case float64:
		return x != 0
	default:
		return true
	}
}
