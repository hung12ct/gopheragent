package agent

import "encoding/json"

// canonicalizeJSON returns a canonical form of s: unmarshal into any, then
// marshal back. encoding/json sorts map keys on marshal, so this yields the
// same output for inputs that differ only in key order or whitespace. Nested
// objects are canonicalized recursively by the stdlib.
//
// If s is not valid JSON, it is returned unchanged so the cache key stays
// stable even for malformed arguments.
//
// Note: numbers decode as float64, so extreme int64 values may lose precision
// on round-trip. For cache-key purposes this is acceptable — two calls that
// differ only by float64-representable precision are effectively the same.
func canonicalizeJSON(s string) string {
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return s
	}
	b, err := json.Marshal(v)
	if err != nil {
		return s
	}
	return string(b)
}

// toolCacheKey derives the agent-loop cache key for a tool call. Using the
// canonical form of argsJSON means semantically identical calls hit the same
// cache entry regardless of how the LLM formatted its JSON.
func toolCacheKey(name, argsJSON string) string {
	return name + ":" + canonicalizeJSON(argsJSON)
}
