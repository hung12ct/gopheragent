package otelllm

import (
	"encoding/json"

	"go.opentelemetry.io/otel/attribute"

	"github.com/hung12ct/gopheragent/pkg/agent"
	"github.com/hung12ct/gopheragent/pkg/history"
	"github.com/hung12ct/gopheragent/pkg/telemetry/semconv"
)

// contentBudgetFields bounds the whole gen_ai.input.messages payload to this
// many max-size fields; older messages beyond it are dropped (newest kept).
const contentBudgetFields = 16

// part and message mirror the GenAI semantic-convention message JSON shape.
type part struct {
	Type      string          `json:"type"`
	Content   string          `json:"content,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	Response  string          `json:"response,omitempty"`
	MIMEType  string          `json:"mime_type,omitempty"`
}

type message struct {
	Role         string `json:"role"`
	Parts        []part `json:"parts"`
	FinishReason string `json:"finish_reason,omitempty"`
}

// inputAttrs serializes the request: system prompts become
// gen_ai.system_instructions, the rest gen_ai.input.messages. Every text field
// is truncated before marshaling so the JSON stays valid, and the history is
// kept newest-first within a total budget so long conversations stay bounded.
func inputAttrs(memory []history.Message, maxBytes int) []attribute.KeyValue {
	var sys []part
	budget := contentBudgetFields * maxBytes
	kept := make([]message, 0, len(memory))
	for i := len(memory) - 1; i >= 0; i-- {
		m := memory[i]
		if m.Role == "system" {
			sys = append(sys, part{Type: "text", Content: semconv.Truncate(m.Content, maxBytes)})
			continue
		}
		msg := convertMessage(m, maxBytes)
		if budget -= messageSize(msg); budget < 0 && len(kept) > 0 {
			break
		}
		kept = append(kept, msg)
	}
	reverse(kept)
	reverse(sys)

	attrs := make([]attribute.KeyValue, 0, 2)
	if len(sys) > 0 {
		attrs = append(attrs, semconv.GenAISystemInstructions.String(mustJSON(sys)))
	}
	return append(attrs, semconv.GenAIInputMessages.String(mustJSON(kept)))
}

// outputAttr serializes the model's reply as gen_ai.output.messages.
func outputAttr(res agent.LLMResult, maxBytes int) attribute.KeyValue {
	msg := message{Role: "assistant", FinishReason: "stop"}
	if res.Content != "" {
		msg.Parts = append(msg.Parts, part{Type: "text", Content: semconv.Truncate(res.Content, maxBytes)})
	}
	for _, tc := range res.ToolCalls {
		msg.Parts = append(msg.Parts, part{Type: "tool_call", ID: tc.ID, Name: tc.Name, Arguments: toolArgs(tc.ArgsJSON, maxBytes)})
	}
	if len(res.ToolCalls) > 0 {
		msg.FinishReason = "tool_call"
	}
	return semconv.GenAIOutputMessages.String(mustJSON([]message{msg}))
}

func convertMessage(m history.Message, maxBytes int) message {
	msg := message{Role: m.Role}
	if m.Role == "tool" {
		msg.Parts = []part{{Type: "tool_call_response", ID: m.ToolCallID, Response: semconv.Truncate(m.Content, maxBytes)}}
		return msg
	}
	if m.Content != "" {
		msg.Parts = append(msg.Parts, part{Type: "text", Content: semconv.Truncate(m.Content, maxBytes)})
	}
	for _, mp := range m.Parts {
		if mp.Type == history.PartText {
			msg.Parts = append(msg.Parts, part{Type: "text", Content: semconv.Truncate(mp.Text, maxBytes)})
			continue
		}
		// Media bytes are never captured — only the fact and type of the part.
		msg.Parts = append(msg.Parts, part{Type: string(mp.Type), MIMEType: mp.MIME})
	}
	for _, tc := range m.ToolCalls {
		msg.Parts = append(msg.Parts, part{Type: "tool_call", ID: tc.ID, Name: tc.Name, Arguments: toolArgs(tc.Arguments, maxBytes)})
	}
	return msg
}

// toolArgs embeds valid, in-budget JSON arguments as an object; anything else
// is carried as a truncated JSON string.
func toolArgs(raw string, maxBytes int) json.RawMessage {
	if raw == "" {
		return nil
	}
	if (maxBytes <= 0 || len(raw) <= maxBytes) && json.Valid([]byte(raw)) {
		return json.RawMessage(raw)
	}
	b, _ := json.Marshal(semconv.Truncate(raw, maxBytes))
	return b
}

func messageSize(m message) int {
	n := 0
	for _, p := range m.Parts {
		n += len(p.Content) + len(p.Response) + len(p.Arguments)
	}
	return n
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "[]"
	}
	return string(b)
}

func reverse[T any](s []T) {
	for i, j := 0, len(s)-1; i < j; i, j = i+1, j-1 {
		s[i], s[j] = s[j], s[i]
	}
}
