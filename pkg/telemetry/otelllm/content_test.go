package otelllm_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"

	"github.com/hung12ct/gopheragent/pkg/agent"
	"github.com/hung12ct/gopheragent/pkg/history"
	"github.com/hung12ct/gopheragent/pkg/telemetry/otelllm"
	"github.com/hung12ct/gopheragent/pkg/telemetry/semconv"
)

var conversation = []history.Message{
	{Role: "system", Content: "be terse"},
	{Role: "user", Content: "look at this", Parts: []history.MediaPart{{Type: history.PartImage, MIME: "image/png", Data: []byte("SECRETBYTES")}}},
	{Role: "assistant", ToolCalls: []history.ToolCall{{ID: "c1", Name: "search", Arguments: `{"q":"go"}`}}},
	{Role: "tool", ToolCallID: "c1", Content: "result text"},
}

func captureSpan(t *testing.T, memory []history.Message, opts ...otelllm.Option) map[attribute.Key]string {
	t.Helper()
	sr, _, tp, _ := newRecorders(t)
	f := &fakeProvider{res: agent.LLMResult{Content: "answer", ToolCalls: []agent.PendingToolCall{{ID: "c2", Name: "x", ArgsJSON: `{"a":1}`}}}}
	opts = append(opts, otelllm.WithModel("m"), otelllm.WithTracer(tp.Tracer("t")))
	if _, err := otelllm.NewProvider(f, opts...).GenerateStream(context.Background(), memory, nil, nil); err != nil {
		t.Fatalf("GenerateStream: %v", err)
	}
	out := map[attribute.Key]string{}
	for _, a := range sr.Ended()[0].Attributes() {
		out[a.Key] = a.Value.Emit()
	}
	return out
}

func TestContentCapture_OffByDefault(t *testing.T) {
	t.Setenv(semconv.EnvCaptureContent, "")
	got := captureSpan(t, conversation)
	for _, k := range []attribute.Key{semconv.GenAIInputMessages, semconv.GenAIOutputMessages, semconv.GenAISystemInstructions} {
		if _, ok := got[k]; ok {
			t.Fatalf("%s captured without opt-in", k)
		}
	}
}

func TestContentCapture_EnvOnProducesSemconvJSON(t *testing.T) {
	t.Setenv(semconv.EnvCaptureContent, "true")
	got := captureSpan(t, conversation)

	var sys []map[string]any
	if err := json.Unmarshal([]byte(got[semconv.GenAISystemInstructions]), &sys); err != nil || sys[0]["content"] != "be terse" {
		t.Fatalf("system_instructions = %s (%v)", got[semconv.GenAISystemInstructions], err)
	}
	in := got[semconv.GenAIInputMessages]
	var msgs []map[string]any
	if err := json.Unmarshal([]byte(in), &msgs); err != nil {
		t.Fatalf("input.messages not valid JSON: %v\n%s", err, in)
	}
	if len(msgs) != 3 || msgs[0]["role"] != "user" {
		t.Fatalf("system must be split out, 3 messages expected: %s", in)
	}
	for _, want := range []string{`"mime_type":"image/png"`, `"arguments":{"q":"go"}`, `"type":"tool_call_response"`, `"response":"result text"`} {
		if !strings.Contains(in, want) {
			t.Fatalf("input.messages missing %s: %s", want, in)
		}
	}
	if strings.Contains(in, "SECRETBYTES") || strings.Contains(in, "U0VDUkVU") {
		t.Fatal("media bytes leaked into captured content")
	}
	outMsg := got[semconv.GenAIOutputMessages]
	if !strings.Contains(outMsg, `"finish_reason":"tool_call"`) || !strings.Contains(outMsg, `"content":"answer"`) {
		t.Fatalf("output.messages = %s", outMsg)
	}
}

func TestContentCapture_OptionOverridesEnv(t *testing.T) {
	t.Setenv(semconv.EnvCaptureContent, "true")
	if _, ok := captureSpan(t, conversation, otelllm.WithContentCapture(false))[semconv.GenAIInputMessages]; ok {
		t.Fatal("WithContentCapture(false) must override the env opt-in")
	}
}

func TestContentCapture_TruncatesAndBoundsHistory(t *testing.T) {
	t.Setenv(semconv.EnvCaptureContent, "")
	var long []history.Message
	for i := 0; i < 100; i++ {
		long = append(long, history.Message{Role: "user", Content: strings.Repeat("x", 500)})
	}
	long = append(long, history.Message{Role: "user", Content: "NEWEST"})
	in := captureSpan(t, long, otelllm.WithContentCapture(true), otelllm.WithContentMaxBytes(100))[semconv.GenAIInputMessages]

	var msgs []map[string]any
	if err := json.Unmarshal([]byte(in), &msgs); err != nil {
		t.Fatalf("truncated payload must stay valid JSON: %v", err)
	}
	if len(msgs) >= 101 || !strings.Contains(in, "NEWEST") {
		t.Fatalf("budget should drop oldest, keep newest: %d messages", len(msgs))
	}
	if strings.Contains(in, strings.Repeat("x", 101)) {
		t.Fatal("field exceeded WithContentMaxBytes")
	}
}
