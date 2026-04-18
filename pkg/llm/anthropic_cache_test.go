package llm

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
)

// Tests cover the pure translation from history.Message.CacheHint to
// Anthropic prompt-cache breakpoints. A live GenerateStream round-trip
// would require mocking the streaming SDK, which is overkill for what
// is a struct-shape transformation.

func TestStampCacheControl_TextBlock(t *testing.T) {
	block := anthropic.NewTextBlock("long system prompt")
	if marshalContains(t, block, "cache_control") {
		t.Fatal("new text block should not carry cache_control by default")
	}
	stampCacheControl(&block)
	if !marshalContains(t, block, `"cache_control":{"type":"ephemeral"}`) {
		t.Fatalf("stamped text block missing ephemeral cache_control: %s", marshalJSON(t, block))
	}
}

func TestStampCacheControl_ToolUseBlock(t *testing.T) {
	block := anthropic.ContentBlockParamUnion{
		OfToolUse: &anthropic.ToolUseBlockParam{
			ID:    "t1",
			Name:  "do_thing",
			Input: map[string]any{"x": 1},
		},
	}
	stampCacheControl(&block)
	if !marshalContains(t, block, `"cache_control":{"type":"ephemeral"}`) {
		t.Fatalf("stamped tool_use missing cache_control: %s", marshalJSON(t, block))
	}
}

func TestStampCacheControl_ToolResultBlock(t *testing.T) {
	block := anthropic.NewToolResultBlock("t1", "result text", false)
	stampCacheControl(&block)
	if !marshalContains(t, block, `"cache_control":{"type":"ephemeral"}`) {
		t.Fatalf("stamped tool_result missing cache_control: %s", marshalJSON(t, block))
	}
}

func TestStampCacheControl_UnknownBlockNoCrash(t *testing.T) {
	// Any block type we don't explicitly handle should be a silent no-op
	// (rather than a panic), so callers stay safe if the SDK adds new kinds.
	block := anthropic.ContentBlockParamUnion{}
	stampCacheControl(&block) // no-op, no panic
}

func TestSystemTextBlockParam_CacheControlJSON(t *testing.T) {
	// The adapter sets CacheControl directly on the system TextBlockParam.
	// Confirm this marshals to the documented wire format.
	block := anthropic.TextBlockParam{
		Text:         "very long system prompt",
		CacheControl: anthropic.NewCacheControlEphemeralParam(),
	}
	data := marshalJSON(t, block)
	if !strings.Contains(data, `"cache_control":{"type":"ephemeral"}`) {
		t.Fatalf("system block cache_control wire format wrong: %s", data)
	}
}

func marshalJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

func marshalContains(t *testing.T, v any, substr string) bool {
	return strings.Contains(marshalJSON(t, v), substr)
}
