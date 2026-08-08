package form

import (
	"fmt"
	"math"
)

// Each typed FieldBuilder wraps a *Field and the parent *Form (so it can
// compile declarative visibility predicates against sibling fields). The
// methods return the builder itself to support fluent chaining.

// ---------------- String ----------------

type StringField struct {
	f    *Field
	form *Form
}

func newStringField(form *Form, name string, ptr *string) *StringField {
	f := &Field{
		Name: name,
		Kind: KindString,
		ptr:  ptr,
		get:  func() any { return *ptr },
		set: func(v any) error {
			s, ok := v.(string)
			if !ok {
				return fmt.Errorf("expected string, got %T", v)
			}
			*ptr = s
			return nil
		},
	}
	return &StringField{f: f, form: form}
}

func (b *StringField) Label(s string) *StringField   { b.f.Label = s; return b }
func (b *StringField) Help(s string) *StringField    { b.f.Help = s; return b }
func (b *StringField) Required() *StringField        { b.f.Required = true; return b }
func (b *StringField) Default(v string) *StringField { b.f.Default = v; return b }
func (b *StringField) ShowWhen(p func() bool) *StringField {
	b.f.visible = p
	return b
}
func (b *StringField) ShowWhenEq(field string, value any) *StringField {
	return b.declarative(field, "eq", value)
}
func (b *StringField) ShowWhenIn(field string, values ...any) *StringField {
	return b.declarative(field, "in", values)
}
func (b *StringField) ShowWhenTruthy(field string) *StringField {
	return b.declarative(field, "truthy", nil)
}
func (b *StringField) Validate(fn func(string) error) *StringField {
	b.f.validate = func(v any) error { return fn(v.(string)) }
	return b
}
func (b *StringField) declarative(field, op string, value any) *StringField {
	vw := &VisibleWhen{Field: field, Op: op, Value: value}
	b.f.visibleWhen = vw
	b.f.visible = b.form.compileVisibleWhen(vw)
	return b
}

// ---------------- Int ----------------

type IntField struct {
	f    *Field
	form *Form

	// Min/Max bounds and the user's custom validator are stored separately
	// from f.validate so each setter (Min/Max/Validate) can be called in any
	// order without overwriting the others. rebuild() re-installs a single
	// composite validator on the field each time any of them changes.
	min      *int
	max      *int
	userFunc func(int) error
}

func newIntField(form *Form, name string, ptr *int) *IntField {
	f := &Field{
		Name: name,
		Kind: KindInt,
		ptr:  ptr,
		get:  func() any { return *ptr },
		set: func(v any) error {
			n, err := toInt(v)
			if err != nil {
				return err
			}
			*ptr = n
			return nil
		},
	}
	return &IntField{f: f, form: form}
}

func (b *IntField) Label(s string) *IntField         { b.f.Label = s; return b }
func (b *IntField) Help(s string) *IntField          { b.f.Help = s; return b }
func (b *IntField) Required() *IntField              { b.f.Required = true; return b }
func (b *IntField) Default(v int) *IntField          { b.f.Default = v; return b }
func (b *IntField) ShowWhen(p func() bool) *IntField { b.f.visible = p; return b }
func (b *IntField) ShowWhenEq(field string, value any) *IntField {
	return b.declarative(field, "eq", value)
}
func (b *IntField) ShowWhenIn(field string, values ...any) *IntField {
	return b.declarative(field, "in", values)
}
func (b *IntField) ShowWhenTruthy(field string) *IntField {
	return b.declarative(field, "truthy", nil)
}

// Min sets an inclusive lower bound. Composes with Max and Validate.
func (b *IntField) Min(n int) *IntField {
	v := n
	b.min = &v
	b.rebuild()
	return b
}

// Max sets an inclusive upper bound. Composes with Min and Validate.
func (b *IntField) Max(n int) *IntField {
	v := n
	b.max = &v
	b.rebuild()
	return b
}

// Validate installs a custom validator. Runs after Min/Max checks.
func (b *IntField) Validate(fn func(int) error) *IntField {
	b.userFunc = fn
	b.rebuild()
	return b
}

// rebuild re-installs a single composite validator on the underlying
// Field that runs Min, Max, then the user validator (whichever are
// configured). Called whenever any of those change.
func (b *IntField) rebuild() {
	if b.min == nil && b.max == nil && b.userFunc == nil {
		b.f.validate = nil
		return
	}
	b.f.validate = func(v any) error {
		n, err := toInt(v)
		if err != nil {
			return err
		}
		if b.min != nil && n < *b.min {
			return fmt.Errorf("must be >= %d", *b.min)
		}
		if b.max != nil && n > *b.max {
			return fmt.Errorf("must be <= %d", *b.max)
		}
		if b.userFunc != nil {
			return b.userFunc(n)
		}
		return nil
	}
}

func (b *IntField) declarative(field, op string, value any) *IntField {
	vw := &VisibleWhen{Field: field, Op: op, Value: value}
	b.f.visibleWhen = vw
	b.f.visible = b.form.compileVisibleWhen(vw)
	return b
}

// ---------------- Float ----------------

type FloatField struct {
	f    *Field
	form *Form

	// See IntField for the rationale behind the split min/max/userFunc
	// storage with a rebuild() composite validator.
	min      *float64
	max      *float64
	userFunc func(float64) error
}

func newFloatField(form *Form, name string, ptr *float64) *FloatField {
	f := &Field{
		Name: name,
		Kind: KindFloat,
		ptr:  ptr,
		get:  func() any { return *ptr },
		set: func(v any) error {
			n, err := toFloat(v)
			if err != nil {
				return err
			}
			*ptr = n
			return nil
		},
	}
	return &FloatField{f: f, form: form}
}

func (b *FloatField) Label(s string) *FloatField         { b.f.Label = s; return b }
func (b *FloatField) Help(s string) *FloatField          { b.f.Help = s; return b }
func (b *FloatField) Required() *FloatField              { b.f.Required = true; return b }
func (b *FloatField) Default(v float64) *FloatField      { b.f.Default = v; return b }
func (b *FloatField) ShowWhen(p func() bool) *FloatField { b.f.visible = p; return b }
func (b *FloatField) ShowWhenEq(field string, value any) *FloatField {
	return b.declarative(field, "eq", value)
}
func (b *FloatField) ShowWhenIn(field string, values ...any) *FloatField {
	return b.declarative(field, "in", values)
}
func (b *FloatField) ShowWhenTruthy(field string) *FloatField {
	return b.declarative(field, "truthy", nil)
}

// Min sets an inclusive lower bound. Composes with Max and Validate.
func (b *FloatField) Min(n float64) *FloatField {
	v := n
	b.min = &v
	b.rebuild()
	return b
}

// Max sets an inclusive upper bound. Composes with Min and Validate.
func (b *FloatField) Max(n float64) *FloatField {
	v := n
	b.max = &v
	b.rebuild()
	return b
}

// Validate installs a custom validator. Runs after Min/Max checks.
func (b *FloatField) Validate(fn func(float64) error) *FloatField {
	b.userFunc = fn
	b.rebuild()
	return b
}

func (b *FloatField) rebuild() {
	if b.min == nil && b.max == nil && b.userFunc == nil {
		b.f.validate = nil
		return
	}
	b.f.validate = func(v any) error {
		n, err := toFloat(v)
		if err != nil {
			return err
		}
		if b.min != nil && n < *b.min {
			return fmt.Errorf("must be >= %g", *b.min)
		}
		if b.max != nil && n > *b.max {
			return fmt.Errorf("must be <= %g", *b.max)
		}
		if b.userFunc != nil {
			return b.userFunc(n)
		}
		return nil
	}
}

func (b *FloatField) declarative(field, op string, value any) *FloatField {
	vw := &VisibleWhen{Field: field, Op: op, Value: value}
	b.f.visibleWhen = vw
	b.f.visible = b.form.compileVisibleWhen(vw)
	return b
}

// ---------------- Bool ----------------

type BoolField struct {
	f    *Field
	form *Form
}

func newBoolField(form *Form, name string, ptr *bool) *BoolField {
	f := &Field{
		Name: name,
		Kind: KindBool,
		ptr:  ptr,
		get:  func() any { return *ptr },
		set: func(v any) error {
			b, ok := v.(bool)
			if !ok {
				return fmt.Errorf("expected bool, got %T", v)
			}
			*ptr = b
			return nil
		},
	}
	return &BoolField{f: f, form: form}
}

func (b *BoolField) Label(s string) *BoolField         { b.f.Label = s; return b }
func (b *BoolField) Help(s string) *BoolField          { b.f.Help = s; return b }
func (b *BoolField) Required() *BoolField              { b.f.Required = true; return b }
func (b *BoolField) Default(v bool) *BoolField         { b.f.Default = v; return b }
func (b *BoolField) ShowWhen(p func() bool) *BoolField { b.f.visible = p; return b }
func (b *BoolField) ShowWhenEq(field string, value any) *BoolField {
	return b.declarative(field, "eq", value)
}
func (b *BoolField) ShowWhenIn(field string, values ...any) *BoolField {
	return b.declarative(field, "in", values)
}
func (b *BoolField) ShowWhenTruthy(field string) *BoolField {
	return b.declarative(field, "truthy", nil)
}
func (b *BoolField) Validate(fn func(bool) error) *BoolField {
	b.f.validate = func(v any) error { return fn(v.(bool)) }
	return b
}
func (b *BoolField) declarative(field, op string, value any) *BoolField {
	vw := &VisibleWhen{Field: field, Op: op, Value: value}
	b.f.visibleWhen = vw
	b.f.visible = b.form.compileVisibleWhen(vw)
	return b
}

// ---------------- Select ----------------

type SelectField struct {
	f    *Field
	form *Form
}

func newSelectField(form *Form, name string, ptr *string) *SelectField {
	f := &Field{
		Name: name,
		Kind: KindSelect,
		ptr:  ptr,
		get:  func() any { return *ptr },
		set: func(v any) error {
			s, ok := v.(string)
			if !ok {
				return fmt.Errorf("expected string, got %T", v)
			}
			*ptr = s
			return nil
		},
	}
	return &SelectField{f: f, form: form}
}

func (b *SelectField) Label(s string) *SelectField   { b.f.Label = s; return b }
func (b *SelectField) Help(s string) *SelectField    { b.f.Help = s; return b }
func (b *SelectField) Required() *SelectField        { b.f.Required = true; return b }
func (b *SelectField) Default(v string) *SelectField { b.f.Default = v; return b }
func (b *SelectField) Option(value, label string) *SelectField {
	b.f.Options = append(b.f.Options, SelectOption{Value: value, Label: label})
	return b
}
func (b *SelectField) ShowWhen(p func() bool) *SelectField { b.f.visible = p; return b }
func (b *SelectField) ShowWhenEq(field string, value any) *SelectField {
	return b.declarative(field, "eq", value)
}
func (b *SelectField) ShowWhenIn(field string, values ...any) *SelectField {
	return b.declarative(field, "in", values)
}
func (b *SelectField) ShowWhenTruthy(field string) *SelectField {
	return b.declarative(field, "truthy", nil)
}
func (b *SelectField) Validate(fn func(string) error) *SelectField {
	b.f.validate = func(v any) error { return fn(v.(string)) }
	return b
}
func (b *SelectField) declarative(field, op string, value any) *SelectField {
	vw := &VisibleWhen{Field: field, Op: op, Value: value}
	b.f.visibleWhen = vw
	b.f.visible = b.form.compileVisibleWhen(vw)
	return b
}

// ---------------- File ----------------

type FileField struct {
	f    *Field
	form *Form
}

func newFileField(form *Form, name string, ptr *string) *FileField {
	f := &Field{
		Name: name,
		Kind: KindFile,
		ptr:  ptr,
		get:  func() any { return *ptr },
		set: func(v any) error {
			s, ok := v.(string)
			if !ok {
				return fmt.Errorf("expected string, got %T", v)
			}
			*ptr = s
			return nil
		},
	}
	return &FileField{f: f, form: form}
}

func (b *FileField) Label(s string) *FileField         { b.f.Label = s; return b }
func (b *FileField) Help(s string) *FileField          { b.f.Help = s; return b }
func (b *FileField) Required() *FileField              { b.f.Required = true; return b }
func (b *FileField) Default(v string) *FileField       { b.f.Default = v; return b }
func (b *FileField) Tag(tag string) *FileField         { b.f.FileTag = tag; return b }
func (b *FileField) ShowWhen(p func() bool) *FileField { b.f.visible = p; return b }
func (b *FileField) ShowWhenEq(field string, value any) *FileField {
	return b.declarative(field, "eq", value)
}
func (b *FileField) ShowWhenIn(field string, values ...any) *FileField {
	return b.declarative(field, "in", values)
}
func (b *FileField) ShowWhenTruthy(field string) *FileField {
	return b.declarative(field, "truthy", nil)
}
func (b *FileField) Validate(fn func(string) error) *FileField {
	b.f.validate = func(v any) error { return fn(v.(string)) }
	return b
}

// ValidateFile registers a content validator for this file field. The
// callback receives the resolved absolute path (filesDirectory joined
// with the bound filename) so it can open and inspect the file
// directly. It runs as part of Schema.Validate, after the FileTag
// check passes and only when the field is visible and non-empty.
//
// Use this for module-specific structural checks the generic file
// tags can't express — e.g. "must be a 7-column CSV" or "first row
// must equal a known header".
func (b *FileField) ValidateFile(fn func(absPath string) error) *FileField {
	b.f.fileValidate = fn
	return b
}
func (b *FileField) declarative(field, op string, value any) *FileField {
	vw := &VisibleWhen{Field: field, Op: op, Value: value}
	b.f.visibleWhen = vw
	b.f.visible = b.form.compileVisibleWhen(vw)
	return b
}

// ---------------- StringList ----------------

type StringListField struct {
	f    *Field
	form *Form
}

func newStringListField(form *Form, name string, ptr *[]string) *StringListField {
	f := &Field{
		Name: name,
		Kind: KindStringList,
		ptr:  ptr,
		get: func() any {
			// Always return a non-nil slice so the JSON round-trip produces
			// "[]" instead of "null" for empty lists.
			if *ptr == nil {
				return []string{}
			}
			return *ptr
		},
		set: func(v any) error {
			list, err := toStringList(v)
			if err != nil {
				return err
			}
			*ptr = list
			return nil
		},
	}
	return &StringListField{f: f, form: form}
}

func (b *StringListField) Label(s string) *StringListField     { b.f.Label = s; return b }
func (b *StringListField) Help(s string) *StringListField      { b.f.Help = s; return b }
func (b *StringListField) Required() *StringListField          { b.f.Required = true; return b }
func (b *StringListField) Default(v []string) *StringListField { b.f.Default = v; return b }
func (b *StringListField) ShowWhen(p func() bool) *StringListField {
	b.f.visible = p
	return b
}
func (b *StringListField) ShowWhenEq(field string, value any) *StringListField {
	return b.declarative(field, "eq", value)
}
func (b *StringListField) ShowWhenIn(field string, values ...any) *StringListField {
	return b.declarative(field, "in", values)
}
func (b *StringListField) ShowWhenTruthy(field string) *StringListField {
	return b.declarative(field, "truthy", nil)
}
func (b *StringListField) Validate(fn func([]string) error) *StringListField {
	b.f.validate = func(v any) error { return fn(v.([]string)) }
	return b
}
func (b *StringListField) declarative(field, op string, value any) *StringListField {
	vw := &VisibleWhen{Field: field, Op: op, Value: value}
	b.f.visibleWhen = vw
	b.f.visible = b.form.compileVisibleWhen(vw)
	return b
}

// ---------------- conversion helpers ----------------

// toInt converts JSON-decoded values (which arrive as float64) and plain ints
// to int, with bounds checking against the platform int range.
func toInt(v any) (int, error) {
	switch x := v.(type) {
	case int:
		return x, nil
	case int32:
		return int(x), nil
	case int64:
		if x > math.MaxInt || x < math.MinInt {
			return 0, fmt.Errorf("value %d overflows int", x)
		}
		return int(x), nil
	case float64:
		if x != math.Trunc(x) {
			return 0, fmt.Errorf("expected integer, got fractional %g", x)
		}
		if x > math.MaxInt || x < math.MinInt {
			return 0, fmt.Errorf("value %g overflows int", x)
		}
		return int(x), nil
	case float32:
		return toInt(float64(x))
	default:
		return 0, fmt.Errorf("cannot convert %T to int", v)
	}
}

// toStringList accepts:
//   - []string (returned as-is)
//   - []any of strings (the shape JSON unmarshal produces for a JSON array)
//   - nil (returned as empty []string)
//
// Any other type or a slice containing a non-string element is an error.
func toStringList(v any) ([]string, error) {
	if v == nil {
		return []string{}, nil
	}
	switch x := v.(type) {
	case []string:
		out := make([]string, len(x))
		copy(out, x)
		return out, nil
	case []any:
		out := make([]string, 0, len(x))
		for i, el := range x {
			s, ok := el.(string)
			if !ok {
				return nil, fmt.Errorf("element %d: expected string, got %T", i, el)
			}
			out = append(out, s)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("cannot convert %T to []string", v)
	}
}

// toFloat converts JSON-decoded numeric values and ints to float64.
func toFloat(v any) (float64, error) {
	switch x := v.(type) {
	case float64:
		return x, nil
	case float32:
		return float64(x), nil
	case int:
		return float64(x), nil
	case int32:
		return float64(x), nil
	case int64:
		return float64(x), nil
	default:
		return 0, fmt.Errorf("cannot convert %T to float64", v)
	}
}
