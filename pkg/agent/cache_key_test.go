package agent

import "testing"

func TestCanonicalizeJSON_KeyOrder(t *testing.T) {
	a := canonicalizeJSON(`{"a":1,"b":2}`)
	b := canonicalizeJSON(`{"b":2,"a":1}`)
	if a != b {
		t.Fatalf("key order should not affect canonical form: %q vs %q", a, b)
	}
}

func TestCanonicalizeJSON_Whitespace(t *testing.T) {
	a := canonicalizeJSON(`{"a":1,"b":2}`)
	b := canonicalizeJSON(`{ "a" : 1 , "b" : 2 }`)
	if a != b {
		t.Fatalf("whitespace should not affect canonical form: %q vs %q", a, b)
	}
}

func TestCanonicalizeJSON_NestedObject(t *testing.T) {
	a := canonicalizeJSON(`{"a":{"z":1,"y":2}}`)
	b := canonicalizeJSON(`{"a":{"y":2,"z":1}}`)
	if a != b {
		t.Fatalf("nested keys should canonicalize recursively: %q vs %q", a, b)
	}
}

func TestCanonicalizeJSON_NonJSONPassthrough(t *testing.T) {
	in := `not json at all`
	if out := canonicalizeJSON(in); out != in {
		t.Fatalf("non-JSON input should round-trip unchanged: %q -> %q", in, out)
	}
}

func TestCanonicalizeJSON_EmptyString(t *testing.T) {
	if out := canonicalizeJSON(""); out != "" {
		t.Fatalf("empty input should return empty, got %q", out)
	}
}

func TestToolCacheKey_Stable(t *testing.T) {
	a := toolCacheKey("websearch", `{"q":"hi","limit":5}`)
	b := toolCacheKey("websearch", `{"limit":5,"q":"hi"}`)
	if a != b {
		t.Fatalf("tool cache key unstable across key order: %q vs %q", a, b)
	}
}

func TestToolCacheKey_DifferentName(t *testing.T) {
	if toolCacheKey("a", `{"q":1}`) == toolCacheKey("b", `{"q":1}`) {
		t.Fatal("different tool names must produce different keys")
	}
}
