// Package openai implements agent.LLMProvider for the OpenAI Chat
// Completions API and any OpenAI-compatible endpoint (Ollama, Groq,
// Together AI, vLLM, ...), plus the OpenAI-backed embedder, vision
// analyzer, and history summary provider.
package openai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strings"

	"github.com/hung12ct/gopheragent/pkg/agent"
	"github.com/hung12ct/gopheragent/pkg/history"
	"github.com/hung12ct/gopheragent/pkg/tools"
	"github.com/sashabaranov/go-openai"
)

// jsonSchemaMarshaler adapts a map[string]any to json.Marshaler so it can be
// handed to openai.ChatCompletionResponseFormatJSONSchema.Schema.
type jsonSchemaMarshaler map[string]any

func (m jsonSchemaMarshaler) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]any(m))
}

// Provider implements agent.LLMProvider using the OpenAI Chat Completions API.
type Provider struct {
	client      *openai.Client
	model       string
	temperature *float64
	topP        *float64
	seed        *int64
	jsonMode    JSONMode
	imageInput  bool
	cfg         clientConfig
}

var _ agent.CapabilityProvider = (*Provider)(nil)

// WithTemperature pins the sampling temperature (0.0–2.0 for OpenAI).
// Use 0 for maximally reproducible classification/extraction turns; unset
// keeps the provider default. An explicit 0 is honored (the SDK's omitempty
// would otherwise drop it and silently revert to the provider default).
func WithTemperature(t float64) Option {
	return providerOptionFunc(func(p *Provider) { p.temperature = &t })
}

// WithTopP pins nucleus sampling. OpenAI recommends adjusting either
// temperature or top_p, not both. Unset keeps the provider default.
func WithTopP(v float64) Option {
	return providerOptionFunc(func(p *Provider) { p.topP = &v })
}

// WithSeed requests best-effort deterministic sampling. Determinism is not
// guaranteed across model versions or backend changes — pair with a pinned
// temperature for the strongest reproducibility OpenAI offers.
func WithSeed(n int64) Option {
	return providerOptionFunc(func(p *Provider) { p.seed = &n })
}

// Capabilities reports what this provider's endpoint accepts, as configured.
//
// New reports api.openai.com's features. NewCompat claims nothing until the
// caller declares its endpoint with WithJSONMode / WithImageInput: a
// compatible endpoint implements an unknown subset, and the whole value of
// this signal is that a consumer can reject an unsuitable provider at
// construction — a blanket claim on behalf of every gateway in the ecosystem
// would hand that consumer a confident wrong answer instead.
func (p *Provider) Capabilities() agent.LLMCapabilities {
	return agent.LLMCapabilities{
		ImageInput:       p.imageInput,
		StructuredOutput: p.jsonMode != JSONModeNone,
	}
}

// resolveAPIKey falls back to OPENAI_API_KEY when apiKey is empty.
func resolveAPIKey(apiKey string) string {
	if apiKey == "" {
		return os.Getenv("OPENAI_API_KEY")
	}
	return apiKey
}

// New creates a wrapper over OpenAI's API.
// Auto-discovers OPENAI_API_KEY from environment if apiKey is empty.
//
// Pass WithBaseURL to target an OpenAI-compatible endpoint instead;
// NewCompat is the same thing with baseURL required rather than optional.
func New(apiKey string, model string, opts ...Option) (*Provider, error) {
	apiKey = resolveAPIKey(apiKey)
	if apiKey == "" {
		return nil, errors.New("openai: New: OPENAI_API_KEY is not set in environment")
	}
	if model == "" {
		model = openai.GPT4o
	}
	// jsonMode's zero value is JSONModeSchema, which is what api.openai.com
	// implements; NewCompat prepends overrides for both defaults.
	p := &Provider{model: model, imageInput: true}
	for _, opt := range opts {
		opt.applyProvider(p)
	}
	client, err := p.cfg.newClient(apiKey)
	if err != nil {
		return nil, fmt.Errorf("openai: New: %w", err)
	}
	p.client = client
	return p, nil
}

// GenerateStream maps GopherAgent history to OpenAI API format and triggers streaming generation.
func (p *Provider) GenerateStream(ctx context.Context, memory []history.Message, availableTools *tools.Registry, streamChan chan<- agent.StreamEvent) (agent.LLMResult, error) {
	reqMessages := make([]openai.ChatCompletionMessage, 0, len(memory))

	for _, m := range memory {
		msg := openai.ChatCompletionMessage{
			Role:    m.Role,
			Content: m.Content,
		}
		// Multimodal: when Parts is set we must populate MultiContent
		// instead of Content (OpenAI rejects both being set on the same
		// message — see ErrContentFieldsMisused in the SDK).
		//
		// Only user messages may carry media. Parts on any other role are
		// rejected rather than dropped: OpenAI has no multimodal assistant or
		// tool content, so silently sending the text alone would answer from
		// media the model never received.
		if len(m.Parts) > 0 {
			if m.Role != "user" {
				return agent.LLMResult{}, fmt.Errorf("openai: %w: %s message carries %d media parts, which this API accepts only on user messages",
					agent.ErrUnrenderablePart, m.Role, len(m.Parts))
			}
			multi, err := partsFromMediaParts(m.Content, m.Parts)
			if err != nil {
				return agent.LLMResult{}, err
			}
			msg.Content = ""
			msg.MultiContent = multi
		}
		// tool result: needs ToolCallID
		if m.Role == "tool" && m.ToolCallID != "" {
			msg.ToolCallID = m.ToolCallID
		}
		// assistant message that invoked tools: needs ToolCalls array
		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			for _, tc := range m.ToolCalls {
				msg.ToolCalls = append(msg.ToolCalls, openai.ToolCall{
					ID:   tc.ID,
					Type: openai.ToolTypeFunction,
					Function: openai.FunctionCall{
						Name:      tc.Name,
						Arguments: tc.Arguments,
					},
				})
			}
		}
		// gpt-4.1 / gpt-5 reject any message whose `content` field marshals
		// to null with "Invalid value for 'content': expected a string, got
		// null." The Go SDK tags Content with `omitempty`, so empty-string
		// content vanishes from the JSON entirely. Stamp a single space
		// whenever the message has no string content and no multimodal
		// parts — applies to every role (assistant w/ or w/o tool calls,
		// tool results, user, system). Model-level no-op; satisfies the
		// validator. Skipped when MultiContent is populated (multimodal).
		if msg.Content == "" && len(msg.MultiContent) == 0 {
			msg.Content = " "
		}
		reqMessages = append(reqMessages, msg)
	}

	var openaiTools []openai.Tool
	if availableTools != nil {
		for _, t := range availableTools.All() {
			desc := t.Descriptor()
			schema := desc.Parameters
			// OpenAI requires "required" to be an array, never null.
			required := schema.Required
			if required == nil {
				required = []string{}
			}
			openaiTools = append(openaiTools, openai.Tool{
				Type: openai.ToolTypeFunction,
				Function: &openai.FunctionDefinition{
					Name:        desc.Name,
					Description: desc.Description,
					Parameters: map[string]any{
						"type":       schema.Type,
						"properties": schema.Properties,
						"required":   required,
					},
				},
			})
		}
	}

	respFormat, reqMessages, err := p.structuredOutputFor(ctx, reqMessages)
	if err != nil {
		return agent.LLMResult{}, err
	}

	req := openai.ChatCompletionRequest{
		Model:          p.model,
		Messages:       reqMessages,
		Tools:          openaiTools,
		ResponseFormat: respFormat,
		Stream:         true,
		StreamOptions: &openai.StreamOptions{
			IncludeUsage: true,
		},
	}
	p.applySampling(&req)
	if effort := reasoningEffortFor(p.model, agent.ThinkingBudgetFromContext(ctx)); effort != "" {
		req.ReasoningEffort = effort
	}
	stream, err := p.client.CreateChatCompletionStream(ctx, req)
	if err != nil {
		return agent.LLMResult{}, fmt.Errorf("openai streaming error: %w", classifyErr(err))
	}
	defer stream.Close()

	var finalContent string
	var usage agent.TokenUsage
	var finishReason openai.FinishReason

	// Accumulate parallel tool calls by index (OpenAI streams them split across chunks)
	type toolCallAccum struct {
		id   string
		name string
		args string
	}
	toolCallMap := map[int]*toolCallAccum{}

	streamChan <- agent.Event(agent.ThoughtEvent{Message: "Analyzing with OpenAI..."})

	for {
		response, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return agent.LLMResult{}, fmt.Errorf("openai chunk read error: %w", classifyErr(err))
		}

		// The final chunk carries Usage when StreamOptions.IncludeUsage is set.
		if response.Usage != nil {
			usage = agent.TokenUsage{
				PromptTokens:     response.Usage.PromptTokens,
				CompletionTokens: response.Usage.CompletionTokens,
				TotalTokens:      response.Usage.TotalTokens,
			}
		}

		if len(response.Choices) == 0 {
			continue
		}
		// The stop reason lands on the final chunk, whose delta is empty —
		// read it before anything below can skip the chunk.
		if r := response.Choices[0].FinishReason; r != "" && r != openai.FinishReasonNull {
			finishReason = r
		}
		delta := response.Choices[0].Delta

		if delta.Content != "" {
			finalContent += delta.Content
			streamChan <- agent.Event(agent.ContentEvent{Text: delta.Content})
		}

		for _, tc := range delta.ToolCalls {
			idx := 0
			if tc.Index != nil {
				idx = *tc.Index
			}
			if toolCallMap[idx] == nil {
				toolCallMap[idx] = &toolCallAccum{}
			}
			if tc.ID != "" {
				toolCallMap[idx].id = tc.ID
			}
			if tc.Function.Name != "" {
				toolCallMap[idx].name = tc.Function.Name
			}
			toolCallMap[idx].args += tc.Function.Arguments
		}
	}

	var pendingCalls []agent.PendingToolCall
	indices := make([]int, 0, len(toolCallMap))
	for k := range toolCallMap {
		indices = append(indices, k)
	}
	sort.Ints(indices)
	for _, idx := range indices {
		tc := toolCallMap[idx]
		pendingCalls = append(pendingCalls, agent.PendingToolCall{
			ID:       tc.id,
			Name:     tc.name,
			ArgsJSON: tc.args,
		})
	}

	result := agent.LLMResult{
		Content:   finalContent,
		ToolCalls: pendingCalls,
		Usage:     usage,
	}
	// A response cut by the token cap or stopped by a content filter is a
	// prefix, not an answer. Returning it as a success is what turns a
	// truncation into a decode error several layers up, naming the
	// caller's schema instead of the real cause. The partial rides on the
	// result so a host can still show what arrived.
	if err := finishReasonErr(finishReason); err != nil {
		if finishReason == openai.FinishReasonLength {
			streamChan <- agent.LimitExhaustedStreamEvent(agent.LimitKindProviderMaxTokens, req.MaxTokens, usage.CompletionTokens)
		}
		return result, err
	}
	return result, nil
}

// applySampling stamps the configured temperature/top_p/seed onto req.
func (p *Provider) applySampling(req *openai.ChatCompletionRequest) {
	if p.temperature != nil {
		req.Temperature = nonZeroFloat32(*p.temperature)
	}
	if p.topP != nil {
		req.TopP = nonZeroFloat32(*p.topP)
	}
	if p.seed != nil {
		seed := int(*p.seed)
		req.Seed = &seed
	}
}

// nonZeroFloat32 maps an explicit 0 to the smallest positive float32. The
// SDK tags Temperature/TopP with omitempty, so a literal 0 vanishes from
// the JSON and the API silently applies its default (1.0) — the epsilon is
// sampling-equivalent to 0 and survives serialization.
func nonZeroFloat32(v float64) float32 {
	if v == 0 {
		return math.SmallestNonzeroFloat32
	}
	return float32(v)
}

// reasoningEffortFor maps a generic ThinkingBudget token count to the
// reasoning_effort string the OpenAI chat API understands.
//
// Only reasoning-capable models receive the field: OpenAI rejects
// reasoning_effort on stock chat models like gpt-4o. The allow-list uses a
// prefix match on names that begin with "o1", "o3", "o4", or carry an
// explicit "reasoning" tag — anything else returns an empty string and the
// caller omits the field entirely.
//
// Thresholds are intentionally coarse: the API exposes only four steps
// ("minimal"/"low"/"medium"/"high"), so a token count is a better UX than
// asking callers to memorize magic strings.
func reasoningEffortFor(model string, budget int) string {
	if budget <= 0 {
		return ""
	}
	m := strings.ToLower(model)
	reasoning := strings.HasPrefix(m, "o1") ||
		strings.HasPrefix(m, "o3") ||
		strings.HasPrefix(m, "o4") ||
		strings.Contains(m, "reasoning")
	if !reasoning {
		return ""
	}
	switch {
	case budget <= 2048:
		return "low"
	case budget <= 8192:
		return "medium"
	default:
		return "high"
	}
}

// partsFromMediaParts converts GopherAgent's provider-neutral MediaPart
// slice into OpenAI's MultiContent representation. A leading text fragment
// derived from Content (when non-empty) keeps captions attached to images.
//
// For image parts, raw Data is folded into a data: URI — OpenAI accepts both
// https URLs and data: URIs interchangeably for image_url.
//
// Any part this adapter cannot render fails the whole message with
// agent.ErrUnrenderablePart rather than being skipped; see that error's doc
// for why silence is the worse outcome. Empty text parts are the one
// exception — they carry nothing to lose.
func partsFromMediaParts(caption string, parts []history.MediaPart) ([]openai.ChatMessagePart, error) {
	out := make([]openai.ChatMessagePart, 0, len(parts)+1)
	if caption != "" {
		out = append(out, openai.ChatMessagePart{
			Type: openai.ChatMessagePartTypeText,
			Text: caption,
		})
	}
	for i, p := range parts {
		switch p.Type {
		case history.PartText:
			if p.Text == "" {
				continue
			}
			out = append(out, openai.ChatMessagePart{
				Type: openai.ChatMessagePartTypeText,
				Text: p.Text,
			})
		case history.PartImage:
			url := p.URL
			if url == "" && len(p.Data) > 0 {
				mime := p.MIME
				if mime == "" {
					mime = "image/png"
				}
				url = "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(p.Data)
			}
			if url == "" {
				return nil, fmt.Errorf("openai: %w: part %d is an image with neither URL nor Data", agent.ErrUnrenderablePart, i)
			}
			out = append(out, openai.ChatMessagePart{
				Type:     openai.ChatMessagePartTypeImageURL,
				ImageURL: &openai.ChatMessageImageURL{URL: url},
			})
		default:
			return nil, fmt.Errorf("openai: %w: part %d has unsupported type %q", agent.ErrUnrenderablePart, i, p.Type)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("openai: %w: %d parts produced no renderable content", agent.ErrUnrenderablePart, len(parts))
	}
	return out, nil
}
