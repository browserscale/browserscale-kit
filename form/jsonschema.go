package form

import (
	"encoding/json"
	"fmt"
)

// JSONSchema returns a JSON Schema (Draft-07) describing this form, suitable
// for driving an external UI (browser, Electron, ...) that knows nothing
// about the in-process Go types.
//
// The schema layout is:
//
//	{
//	  "$schema": "http://json-schema.org/draft-07/schema#",
//	  "title": <DisplayName>,
//	  "x-module": <Module>,
//	  "type": "object",
//	  "properties": {
//	     <fieldName>: {
//	        "type": ..., "title": ..., "default": ..., "x-kind": ..., ...
//	     },
//	     ...
//	  },
//	  "required": [ ... ],
//	  "allOf": [ // conditional visibility, only for fields with declarative ShowWhen
//	    { "if": { "properties": { <ctrl>: { "const": <value> } } },
//	      "then": { "required": [ <field> ] } },
//	    ...
//	  ]
//	}
//
// Fields whose visibility uses a Go predicate (ShowWhen(func() bool)) lose
// their conditional information in the export because closures cannot be
// serialized. For round-tripping with a remote UI, prefer ShowWhenEq /
// ShowWhenIn / ShowWhenTruthy where possible.
func (s *Schema) JSONSchema() ([]byte, error) {
	props := map[string]any{}
	var required []string

	for _, f := range s.Fields {
		props[f.Name] = fieldToJSONSchema(f)
		if f.Required && f.visibleWhen == nil {
			required = append(required, f.Name)
		}
	}

	root := map[string]any{
		"$schema":    "http://json-schema.org/draft-07/schema#",
		"title":      s.DisplayName,
		"x-module":   s.Module,
		"type":       "object",
		"properties": props,
	}
	if len(required) > 0 {
		root["required"] = required
	}
	if conds := conditionalAllOf(s.Fields); len(conds) > 0 {
		root["allOf"] = conds
	}

	return json.MarshalIndent(root, "", "  ")
}

func fieldToJSONSchema(f *Field) map[string]any {
	out := map[string]any{
		"title":  displayOrName(f),
		"x-kind": string(f.Kind),
	}
	if f.Help != "" {
		out["description"] = f.Help
	}
	if f.Default != nil {
		out["default"] = f.Default
	}
	switch f.Kind {
	case KindString:
		out["type"] = "string"
	case KindInt:
		out["type"] = "integer"
	case KindFloat:
		out["type"] = "number"
	case KindBool:
		out["type"] = "boolean"
	case KindSelect:
		out["type"] = "string"
		values := make([]any, 0, len(f.Options))
		labels := make(map[string]string, len(f.Options))
		for _, o := range f.Options {
			values = append(values, o.Value)
			labels[o.Value] = o.Label
		}
		out["enum"] = values
		out["x-enumLabels"] = labels
	case KindFile:
		out["type"] = "string"
		out["format"] = "file-path"
		if f.FileTag != "" {
			out["x-fileTag"] = f.FileTag
		}
	case KindStringList:
		out["type"] = "array"
		out["items"] = map[string]any{"type": "string"}
	}
	return out
}

// conditionalAllOf turns declarative ShowWhen* annotations into JSON
// Schema "if/then" clauses that require the dependent field when the
// controlling field has the specified value.
//
// We deliberately do NOT emit clauses for predicate-based visibility
// because there is no serializable representation of a Go closure.
func conditionalAllOf(fields []*Field) []any {
	var out []any
	for _, f := range fields {
		vw := f.visibleWhen
		if vw == nil {
			continue
		}
		ifClause := map[string]any{}
		switch vw.Op {
		case "eq":
			ifClause = map[string]any{
				"properties": map[string]any{
					vw.Field: map[string]any{"const": vw.Value},
				},
				"required": []string{vw.Field},
			}
		case "ne":
			ifClause = map[string]any{
				"properties": map[string]any{
					vw.Field: map[string]any{"not": map[string]any{"const": vw.Value}},
				},
			}
		case "in":
			ifClause = map[string]any{
				"properties": map[string]any{
					vw.Field: map[string]any{"enum": vw.Value},
				},
				"required": []string{vw.Field},
			}
		case "truthy":
			ifClause = map[string]any{
				"properties": map[string]any{
					vw.Field: map[string]any{"not": map[string]any{"const": nil}},
				},
				"required": []string{vw.Field},
			}
		default:
			continue
		}
		thenClause := map[string]any{}
		if f.Required {
			thenClause["required"] = []string{f.Name}
		}
		out = append(out, map[string]any{
			"if":   ifClause,
			"then": thenClause,
		})
	}
	return out
}

func displayOrName(f *Field) string {
	if f.Label != "" {
		return f.Label
	}
	return f.Name
}

// String returns a pretty-printed JSON Schema. Useful for debugging /
// quick inspection in tests. On error it returns a string of the form
// "<error: ...>".
func (s *Schema) String() string {
	b, err := s.JSONSchema()
	if err != nil {
		return fmt.Sprintf("<error: %s>", err)
	}
	return string(b)
}
