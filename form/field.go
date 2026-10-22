package form

import (
	"fmt"
	"maps"
	"strconv"
	"strings"

	"github.com/browserscale/browserscale-kit/proxy"
)

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
	// (host:port or host:port:user:pass per line), or be a proxy source
	// spec (.json); the value may also name a saved source, proxy:<id>, or
	// browserscale's managed proxies, "managed" or "managed:<country>".
	// Form.Proxies adds such a field with what the bot needs of them.
	TagProxies = "proxies"
)

// What a proxies field asks of the address a browser gets, ProxyRule.Address.
const (
	// AddressFresh: every browser needs a new exit address.
	AddressFresh = "fresh"
	// AddressKeep: an account keeps its own address from job to job.
	AddressKeep = "keep"
)

// ProxyRule is what a proxies field needs of the proxies it's given. Only
// Kinds turns a setting down; the rest are notes beside it.
type ProxyRule struct {
	// Kinds are the kinds of source that fit (proxy.KindList, KindGateway,
	// KindStatic, KindMobile, KindManaged); any when empty.
	Kinds []string `json:"kinds,omitempty"`
	// Address is AddressFresh, AddressKeep or "" for either.
	Address string `json:"address,omitempty"`
	// Country is the exit country the site needs, e.g. "de". Managed
	// proxies and a source with a country parameter get it unless the
	// setting names another.
	Country string `json:"country,omitempty"`
	// Sticky is how long a browser needs to keep its exit address, a
	// duration like "15m"; a source with a parameter for it gets it.
	Sticky string `json:"sticky,omitempty"`
	// Params are the bot's values for source parameters by name, such as a
	// provider's zone; a source without the parameter leaves it out.
	Params map[string]string `json:"params,omitempty"`
	// From are parameters that follow another setting of the form: by
	// meaning or name, the field whose value they take. The field's value,
	// while it shows and isn't empty, takes the place of the bot's own.
	From map[string]string `json:"from,omitempty"`

	// value reads another field of the form while it shows, for From.
	value func(field string) any
}

// Wants are the rule's values for a source's parameters, by meaning
// (proxy.Country, proxy.Sticky) and by name, as proxy.Open takes them:
// the bot's own, and over them the settings From names.
func (r *ProxyRule) Wants() map[string]string {
	if r == nil {
		return nil
	}
	w := map[string]string{}
	for k, v := range r.Params {
		w[k] = v
	}
	if r.Country != "" {
		w[proxy.Country] = r.Country
	}
	if r.Sticky != "" {
		w[proxy.Sticky] = r.Sticky
	}
	if r.value != nil {
		for k, field := range r.From {
			if v := fromValue(k, r.value(field)); v != "" {
				w[k] = v
			}
		}
	}
	return w
}

// fromValue writes a field's value for a parameter: a number of minutes
// for proxy.Sticky as "15m", a country in lower case.
func fromValue(key string, v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		s := strings.TrimSpace(x)
		if key == proxy.Country {
			s = strings.ToLower(s)
		}
		return s
	case bool:
		return ""
	}
	if f, ok := numericToFloat(v); ok {
		if f <= 0 {
			return ""
		}
		s := strconv.FormatFloat(f, 'f', -1, 64)
		if key == proxy.Sticky {
			s += "m"
		}
		return s
	}
	return fmt.Sprint(v)
}

// Current is the rule with the values From takes filled in, as the form
// stands: what a run gets, for showing it.
func (r *ProxyRule) Current() *ProxyRule {
	if r == nil || len(r.From) == 0 {
		return r
	}
	c := *r
	c.Params = maps.Clone(r.Params)
	for k, v := range r.Wants() {
		switch k {
		case proxy.Country:
			c.Country = v
		case proxy.Sticky:
			c.Sticky = v
		default:
			if c.Params == nil {
				c.Params = map[string]string{}
			}
			c.Params[k] = v
		}
	}
	return &c
}

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

	// Section is the heading the field is grouped under, set with
	// Form.Section; empty before the first one.
	Section string
	// Secret marks a string field whose value a renderer shows masked.
	Secret bool
	// Placeholder is a hint shown in an empty text field.
	Placeholder string
	// Min and Max are a number field's bounds, nil when unbounded.
	Min, Max *float64

	// Options populated for Kind == KindSelect.
	Options []SelectOption

	// FileTag populated for Kind == KindFile. Empty means TagRaw.
	FileTag string
	// Proxies is a proxies field's rule, set by Form.Proxies.
	Proxies *ProxyRule

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
