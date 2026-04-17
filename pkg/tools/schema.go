package tools

import (
	"fmt"
	"reflect"
	"strings"
)

// SchemaFor derives a ToolSchema from a Go struct type using reflection.
// It removes the boilerplate of hand-writing map[string]any literals for
// every tool's parameter schema.
//
// # Supported field types
//
//   - string                                        → "string"
//   - bool                                          → "boolean"
//   - all int / uint variants                       → "integer"
//   - float32, float64                              → "number"
//   - slice / array of a supported element type     → "array" with items
//   - struct                                        → nested "object"
//   - pointer to any of the above                   → same type, optional
//
// # Supported struct tags
//
//   - `json:"name"`                 property name (defaults to the Go field name)
//   - `json:"-"`                    skip this field
//   - `json:",omitempty"`           mark the field optional
//   - `description:"..."`           human-readable description sent to the LLM
//   - `enum:"a,b,c"`                restrict the field to a fixed set of values
//   - `required:"true"`/`"false"`   force the required state, overriding omitempty
//
// A field is required by default; it becomes optional when the field type is
// a pointer, the json tag carries `omitempty`, or the `required` tag is
// explicitly set to "false".
//
// # Example
//
//	type SearchArgs struct {
//	    Query string `json:"query" description:"The search query"`
//	    Topic string `json:"topic,omitempty" description:"Search category" enum:"general,news"`
//	    Days  int    `json:"days,omitempty" description:"Limit results to the last N days"`
//	}
//
//	func (t *SearchTool) ParametersSchema() tools.ToolSchema {
//	    return tools.SchemaFor[SearchArgs]()
//	}
//
// SchemaFor panics when T is not a struct type or when a field has an
// unsupported type. This is deliberate: schemas are built at startup, so a
// panic surfaces misuse immediately instead of silently dropping fields the
// LLM would then fail to populate.
func SchemaFor[T any]() ToolSchema {
	var zero T
	t := reflect.TypeOf(zero)
	// Handle SchemaFor[*MyArgs]() by unwrapping the outer pointer.
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == nil || t.Kind() != reflect.Struct {
		panic(fmt.Sprintf("tools.SchemaFor: expected struct type, got %v", t))
	}
	return structSchema(t)
}

// structSchema builds a ToolSchema for a struct type. Called recursively for
// nested struct fields.
func structSchema(t reflect.Type) ToolSchema {
	props := make(map[string]any, t.NumField())
	var required []string

	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name, omitempty := parseJSONTag(f.Tag.Get("json"), f.Name)
		if name == "-" {
			continue
		}

		fieldType := f.Type
		isPtr := false
		if fieldType.Kind() == reflect.Pointer {
			isPtr = true
			fieldType = fieldType.Elem()
		}

		prop, err := fieldSchema(fieldType)
		if err != nil {
			panic(fmt.Sprintf("tools.SchemaFor: field %q: %v", f.Name, err))
		}
		if desc := f.Tag.Get("description"); desc != "" {
			prop["description"] = desc
		}
		if enumTag := f.Tag.Get("enum"); enumTag != "" {
			prop["enum"] = splitEnum(enumTag)
		}
		props[name] = prop

		isOptional := isPtr || omitempty
		switch f.Tag.Get("required") {
		case "true":
			isOptional = false
		case "false":
			isOptional = true
		}
		if !isOptional {
			required = append(required, name)
		}
	}

	return ToolSchema{Type: "object", Properties: props, Required: required}
}

// fieldSchema maps a Go type to its JSON-Schema fragment, excluding tag-derived
// fields (description/enum) which are layered on by the caller.
func fieldSchema(t reflect.Type) (map[string]any, error) {
	switch t.Kind() {
	case reflect.String:
		return map[string]any{"type": "string"}, nil
	case reflect.Bool:
		return map[string]any{"type": "boolean"}, nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return map[string]any{"type": "integer"}, nil
	case reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}, nil
	case reflect.Slice, reflect.Array:
		elem := t.Elem()
		if elem.Kind() == reflect.Pointer {
			elem = elem.Elem()
		}
		item, err := fieldSchema(elem)
		if err != nil {
			return nil, fmt.Errorf("slice element: %w", err)
		}
		return map[string]any{"type": "array", "items": item}, nil
	case reflect.Struct:
		nested := structSchema(t)
		m := map[string]any{"type": "object"}
		if len(nested.Properties) > 0 {
			m["properties"] = nested.Properties
		}
		if len(nested.Required) > 0 {
			m["required"] = nested.Required
		}
		return m, nil
	default:
		return nil, fmt.Errorf("unsupported kind %v", t.Kind())
	}
}

// parseJSONTag returns the property name and whether omitempty is present.
// When the tag is empty or its name part is empty, fallback is used.
func parseJSONTag(tag, fallback string) (name string, omitempty bool) {
	if tag == "" {
		return fallback, false
	}
	parts := strings.Split(tag, ",")
	name = parts[0]
	if name == "" {
		name = fallback
	}
	for _, p := range parts[1:] {
		if p == "omitempty" {
			omitempty = true
		}
	}
	return name, omitempty
}

// splitEnum parses a comma-separated enum tag into a trimmed, non-empty slice.
func splitEnum(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if v := strings.TrimSpace(p); v != "" {
			out = append(out, v)
		}
	}
	return out
}
