package anthropic

import (
	"fmt"
	"slices"
	"strings"
)

// unsupportedSchemaKeywords are JSON-Schema constraints the structured-output
// grammar rejects with a 400. structuredSchema moves them into the field's
// description so the model still sees the intent.
var unsupportedSchemaKeywords = []string{
	"minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum", "multipleOf",
	"minLength", "maxLength", "maxItems", "uniqueItems",
}

// supportedStringFormats are the only "format" values the grammar accepts.
var supportedStringFormats = []string{
	"date-time", "time", "date", "duration", "email", "hostname", "uri", "ipv4", "ipv6", "uuid",
}

// schemaChildMaps and schemaChildLists name the keywords whose values hold
// nested schemas, keyed by name or listed in order.
var (
	schemaChildMaps  = []string{"properties", "$defs", "definitions"}
	schemaChildLists = []string{"anyOf", "allOf", "oneOf", "prefixItems"}
)

// structuredSchema returns a copy of schema that output_config.format
// accepts: every object closed with additionalProperties:false, unsupported
// constraints moved into descriptions, unsupported string formats dropped.
// The caller's schema is left untouched. Callers that rely on a moved
// constraint must validate the result themselves.
func structuredSchema(schema map[string]any) map[string]any {
	out := make(map[string]any, len(schema)+1)
	var moved []string
	for k, v := range schema {
		switch {
		case slices.Contains(unsupportedSchemaKeywords, k):
			moved = append(moved, fmt.Sprintf("%s: %v", k, v))
		case k == "minItems" && !isZeroOrOne(v):
			moved = append(moved, fmt.Sprintf("%s: %v", k, v))
		case k == "format" && !isSupportedFormat(v):
			moved = append(moved, fmt.Sprintf("%s: %v", k, v))
		default:
			out[k] = structuredSchemaValue(k, v)
		}
	}
	if isObjectSchema(out) {
		out["additionalProperties"] = false
	}
	if len(moved) > 0 {
		slices.Sort(moved)
		note := "{" + strings.Join(moved, ", ") + "}"
		if desc, _ := out["description"].(string); desc != "" {
			note = desc + " " + note
		}
		out["description"] = note
	}
	return out
}

// structuredSchemaValue rewrites the nested schemas held under keyword k and
// returns every other value as is.
func structuredSchemaValue(k string, v any) any {
	switch child := v.(type) {
	case map[string]any:
		if !slices.Contains(schemaChildMaps, k) {
			if k == "items" || k == "not" || k == "additionalProperties" {
				return structuredSchema(child)
			}
			return v
		}
		out := make(map[string]any, len(child))
		for name, sub := range child {
			if m, ok := sub.(map[string]any); ok {
				out[name] = structuredSchema(m)
			} else {
				out[name] = sub
			}
		}
		return out
	case []any:
		if !slices.Contains(schemaChildLists, k) {
			return v
		}
		out := make([]any, len(child))
		for i, sub := range child {
			if m, ok := sub.(map[string]any); ok {
				out[i] = structuredSchema(m)
			} else {
				out[i] = sub
			}
		}
		return out
	}
	return v
}

// isObjectSchema reports whether s describes an object, by type or by
// declaring properties.
func isObjectSchema(s map[string]any) bool {
	if _, ok := s["properties"]; ok {
		return true
	}
	switch t := s["type"].(type) {
	case string:
		return t == "object"
	case []any:
		return slices.Contains(t, any("object"))
	case []string:
		return slices.Contains(t, "object")
	}
	return false
}

func isZeroOrOne(v any) bool {
	switch n := v.(type) {
	case int:
		return n == 0 || n == 1
	case int64:
		return n == 0 || n == 1
	case float64:
		return n == 0 || n == 1
	}
	return false
}

func isSupportedFormat(v any) bool {
	s, ok := v.(string)
	return ok && slices.Contains(supportedStringFormats, s)
}
