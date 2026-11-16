// Package anthropic implements agent.LLMProvider for Anthropic's Claude
// models via the Messages API, including prompt caching, adaptive
// thinking, and native structured output.
package anthropic

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/hung12ct/gopheragent/pkg/agent"
	"github.com/hung12ct/gopheragent/pkg/history"
	"github.com/hung12ct/gopheragent/pkg/tools"
)

// DefaultMaxTokens is the per-call Anthropic MaxTokens used when
// no override is provided. 8192 catches typical chat + tool-use turns
// without truncating most code-gen workloads. Bump via WithMaxTokens for
// long structured outputs (HTML5 playables, full schema dumps,
// inline charts) — Sonnet 4.6 supports up to 64K.
const DefaultMaxTokens int64 = 8192

// Provider implements agent.LLMProvider using the Anthropic Messages API (Claude).
type Provider struct {
	client      *anthropic.Client
	model       anthropic.Model
	MaxTokens   int64
	temperature *float64
	topP        *float64
	effort      string
}

var _ agent.CapabilityProvider = (*Provider)(nil)

// Capabilities reports features implemented by the Anthropic adapter.
func (*Provider) Capabilities() agent.LLMCapabilities {
	return agent.LLMCapabilities{ImageInput: true, StructuredOutput: true}
}

// Option configures a Provider at construction.
type Option func(*Provider)

// WithMaxTokens overrides the per-call Anthropic MaxTokens. Use for
// code-generation, long structured output, or any workload where the
// default truncates mid-stream.
func WithMaxTokens(n int64) Option {
	return func(p *Provider) { p.MaxTokens = n }
}

// WithEffort sends output_config.effort verbatim on every request and takes
// precedence over the effort derived from agent.ThinkingBudget. Unset (or "")
// keeps the model's default. The value is not validated: the accepted set
// ("low", "medium", "high", "xhigh", "max") differs per model, and the API
// rejects what a model does not support.
func WithEffort(effort string) Option {
	return func(p *Provider) { p.effort = effort }
}

// WithTemperature pins the sampling temperature (0.0–1.0 for Anthropic).
// Use 0 for maximally reproducible classification/extraction turns; unset
// keeps the provider default. Ignored while thinking is enabled — the API
// rejects sampling overrides alongside thinking. Anthropic exposes no seed
// parameter, so temperature 0 is best-effort reproducibility, not bit-exact.
//
// Models from Claude Opus 4.7 on reject temperature and top_p outright
// ("temperature is deprecated for this model"). The option is sent as given,
// so leave it unset for those models.
func WithTemperature(t float64) Option {
	return func(p *Provider) { p.temperature = &t }
}

// WithTopP pins nucleus sampling. Anthropic recommends adjusting either
// temperature or top_p, not both. Unset keeps the provider default.
// Ignored while thinking is enabled; see WithTemperature for the models that
// reject it.
func WithTopP(v float64) Option {
	return func(p *Provider) { p.topP = &v }
}

// New creates a Claude-backed provider.
// Auto-discovers ANTHROPIC_API_KEY from environment if apiKey is empty.
func New(apiKey string, model string, opts ...Option) (*Provider, error) {
	if apiKey == "" {
		apiKey = os.Getenv("ANTHROPIC_API_KEY")
	}
	if apiKey == "" {
		return nil, errors.New("ANTHROPIC_API_KEY is not set in environment")
	}
	m := anthropic.Model(model)
	if model == "" {
		m = anthropic.ModelClaudeSonnet4_6
	}
	client := anthropic.NewClient(option.WithAPIKey(apiKey))
	p := &Provider{client: &client, model: m, MaxTokens: DefaultMaxTokens}
	for _, opt := range opts {
		opt(p)
	}
	return p, nil
}

func (p *Provider) GenerateStream(ctx context.Context, memory []history.Message, availableTools *tools.Registry, streamChan chan<- agent.StreamEvent) (agent.LLMResult, error) {
	var systemBlocks []anthropic.TextBlockParam
	var messages []anthropic.MessageParam

	// Anthropic rejects requests where an assistant tool_use isn't
	// immediately followed by its matching tool_result (400: "tool_use ids
	// were found without tool_result blocks immediately after"). Adopters
	// who pass a sub-slice of session history into ad-hoc LLM calls
	// (summarisation, classification, custom titling) routinely produce
	// that shape. Sealing dangling tool_use blocks here makes the
	// converter robust regardless of how the input slice was assembled —
	// the same idempotent helper the agent loop already applies to every
	// per-turn history before invoking the provider.
	memory = agent.PatchDanglingToolCalls(memory)

	for _, m := range memory {
		// Only user messages render media. Parts on any other role are
		// rejected rather than dropped: the branches below read Content
		// alone, so passing them through would answer from media the model
		// never received.
		if len(m.Parts) > 0 && m.Role != "user" {
			return agent.LLMResult{}, fmt.Errorf("anthropic: %w: %s message carries %d media parts, which this API accepts only on user messages",
				agent.ErrUnrenderablePart, m.Role, len(m.Parts))
		}
		switch m.Role {
		case "system":
			block := anthropic.TextBlockParam{Text: m.Content}
			if m.CacheHint {
				block.CacheControl = anthropic.NewCacheControlEphemeralParam()
			}
			systemBlocks = append(systemBlocks, block)
		case "user":
			var blocks []anthropic.ContentBlockParamUnion
			if len(m.Parts) > 0 {
				rendered, err := blocksFromMediaParts(m.Content, m.Parts)
				if err != nil {
					return agent.LLMResult{}, err
				}
				blocks = rendered
			} else {
				blocks = []anthropic.ContentBlockParamUnion{anthropic.NewTextBlock(m.Content)}
			}
			if m.CacheHint && len(blocks) > 0 {
				stampCacheControl(&blocks[len(blocks)-1])
			}
			messages = append(messages, anthropic.NewUserMessage(blocks...))
		case "assistant":
			if len(m.ToolCalls) > 0 {
				var blocks []anthropic.ContentBlockParamUnion
				if m.Content != "" {
					blocks = append(blocks, anthropic.NewTextBlock(m.Content))
				}
				for _, tc := range m.ToolCalls {
					var input map[string]any
					if err := json.Unmarshal([]byte(tc.Arguments), &input); err != nil {
						return agent.LLMResult{}, fmt.Errorf("corrupt tool call args for %s: %w", tc.Name, err)
					}
					blocks = append(blocks, anthropic.ContentBlockParamUnion{
						OfToolUse: &anthropic.ToolUseBlockParam{
							ID:    tc.ID,
							Name:  tc.Name,
							Input: input,
						},
					})
				}
				if m.CacheHint && len(blocks) > 0 {
					stampCacheControl(&blocks[len(blocks)-1])
				}
				messages = append(messages, anthropic.MessageParam{Role: anthropic.MessageParamRoleAssistant, Content: blocks})
			} else {
				blocks := []anthropic.ContentBlockParamUnion{anthropic.NewTextBlock(m.Content)}
				if m.CacheHint {
					stampCacheControl(&blocks[0])
				}
				messages = append(messages, anthropic.MessageParam{
					Role:    anthropic.MessageParamRoleAssistant,
					Content: blocks,
				})
			}
		case "tool":
			block := anthropic.NewToolResultBlock(m.ToolCallID, m.Content, m.IsError)
			if m.CacheHint {
				stampCacheControl(&block)
			}
			messages = append(messages, anthropic.NewUserMessage(block))
		}
	}

	var sdkTools []anthropic.ToolUnionParam
	if availableTools != nil {
		for _, t := range availableTools.All() {
			desc := t.Descriptor()
			schema := desc.Parameters
			sdkTools = append(sdkTools, anthropic.ToolUnionParam{
				OfTool: &anthropic.ToolParam{
					Name:        desc.Name,
					Description: anthropic.String(desc.Description),
					InputSchema: anthropic.ToolInputSchemaParam{
						Properties: schema.Properties,
						Required:   schema.Required,
					},
				},
			})
		}
	}

	params := anthropic.MessageNewParams{
		Model:     p.model,
		MaxTokens: p.MaxTokens,
		Messages:  messages,
	}
	if len(systemBlocks) > 0 {
		params.System = systemBlocks
	}
	if len(sdkTools) > 0 {
		params.Tools = sdkTools
	}
	if so := agent.StructuredOutputFromContext(ctx); so != nil && len(so.Schema) > 0 {
		params.OutputConfig.Format = anthropic.JSONOutputFormatParam{Schema: structuredSchema(so.Schema)}
	}
	p.applySampling(&params, p.applyThinking(ctx, &params))

	streamChan <- agent.Event(agent.ThoughtEvent{Message: fmt.Sprintf("Analyzing with Claude (%s)...", p.model)})

	stream := p.client.Messages.NewStreaming(ctx, params)
	defer stream.Close()

	accumulated := anthropic.Message{}
	truncated := false
	for stream.Next() {
		event := stream.Current()
		if err := accumulated.Accumulate(event); err != nil {
			// Accumulate fails when the SDK re-marshals an accumulated
			// tool_use whose Input json.RawMessage was cut off mid-stream
			// (typical when MaxTokens hits inside a large tool argument).
			// If at least one content block already landed, treat this as
			// soft truncation: emit the typed cap event so adopters can
			// raise MaxTokens, and ship whatever's intact. Partial tool_use
			// blocks are dropped during extraction below.
			if len(accumulated.Content) == 0 {
				return agent.LLMResult{}, fmt.Errorf("anthropic accumulate error: %w", classifyErr(err))
			}
			streamChan <- agent.Event(agent.LimitExhaustedEvent{
				Kind:   agent.LimitKindProviderMaxTokens,
				Limit:  int(p.MaxTokens),
				Reason: agent.LimitReasonIncompleteToolUse,
			})
			truncated = true
			break
		}

		switch variant := event.AsAny().(type) {
		case anthropic.ContentBlockDeltaEvent:
			switch delta := variant.Delta.AsAny().(type) {
			case anthropic.TextDelta:
				if delta.Text != "" {
					streamChan <- agent.Event(agent.ContentEvent{Text: delta.Text})
				}
			}
		case anthropic.ContentBlockStopEvent:
			// A content block just finalized. If it was a tool_use block,
			// the full {id, name, input} is available on `accumulated`
			// right now — emit tool_call_ready so the agent loop can
			// speculatively start safe tools while the response keeps
			// streaming. Any downstream consumer that doesn't care simply
			// ignores the event type.
			idx := int(variant.Index)
			if idx >= 0 && idx < len(accumulated.Content) {
				if tu, ok := accumulated.Content[idx].AsAny().(anthropic.ToolUseBlock); ok {
					if argsBytes, err := json.Marshal(tu.Input); err == nil {
						streamChan <- agent.Event(agent.ToolCallReadyEvent{
							ID:       tu.ID,
							Name:     tu.Name,
							ArgsJSON: string(argsBytes),
						})
					}
				}
			}
		}
	}

	if !truncated && stream.Err() != nil {
		return agent.LLMResult{}, fmt.Errorf("anthropic stream error: %w", classifyErr(stream.Err()))
	}

	// Surface the per-call MaxTokens truncation as a typed cap event so
	// adopters can render "model truncated; raise MaxTokens" instead of
	// silently shipping half-rendered code blocks. Skipped when soft
	// truncation already fired the same event above. The typed error is
	// returned after the content is extracted, below.
	if !truncated && accumulated.StopReason == anthropic.StopReasonMaxTokens {
		streamChan <- agent.LimitExhaustedStreamEvent(agent.LimitKindProviderMaxTokens, int(p.MaxTokens), 0)
	}

	var finalContent string
	var pendingCalls []agent.PendingToolCall
	for _, block := range accumulated.Content {
		switch v := block.AsAny().(type) {
		case anthropic.TextBlock:
			finalContent += v.Text
		case anthropic.ToolUseBlock:
			argsBytes, err := json.Marshal(v.Input)
			if err != nil {
				// A complete tool_use always has well-formed Input (the
				// SDK validates at ContentBlockStopEvent), so a marshal
				// failure here means the stream cut off before the block
				// finalized. Drop the partial call — dispatching it would
				// just produce a bad-args tool error.
				continue
			}
			pendingCalls = append(pendingCalls, agent.PendingToolCall{
				ID:       v.ID,
				Name:     v.Name,
				ArgsJSON: string(argsBytes),
			})
		}
	}

	usage := agent.TokenUsage{
		PromptTokens:     int(accumulated.Usage.InputTokens),
		CompletionTokens: int(accumulated.Usage.OutputTokens),
	}
	usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens

	result := agent.LLMResult{Content: finalContent, ToolCalls: pendingCalls, Usage: usage}
	// A capped or refused response is a prefix, not an answer. Returning it
	// as a success is what turns a truncation into a decode error several
	// layers up, naming the caller's schema instead of the real cause. The
	// partial rides on the result so a host can still show what arrived.
	// Soft truncation reports max_tokens too: the SDK could not finalize a
	// tool_use block precisely because the cap fired mid-JSON.
	stopReason := accumulated.StopReason
	if truncated {
		stopReason = anthropic.StopReasonMaxTokens
	}
	if err := stopReasonErr(stopReason); err != nil {
		return result, err
	}
	return result, nil
}

// applySampling stamps the configured temperature/top_p onto params.
// Skipped when thinking is enabled: the API rejects temperature and top_p
// overrides alongside thinking, so thinking wins by construction instead of
// 400ing the call.
func (p *Provider) applySampling(params *anthropic.MessageNewParams, thinkingEnabled bool) {
	if thinkingEnabled {
		return
	}
	if p.temperature != nil {
		params.Temperature = anthropic.Float(*p.temperature)
	}
	if p.topP != nil {
		params.TopP = anthropic.Float(*p.topP)
	}
}

// applyThinking translates agent.ThinkingBudget and WithEffort into the
// request and reports whether thinking was switched on.
//
// Current models take adaptive thinking plus an effort level and reject a
// token budget, so a budget turns adaptive thinking on and picks the effort
// unless WithEffort already did. The models in budgetThinkingModel predate
// that and keep the token budget.
func (p *Provider) applyThinking(ctx context.Context, params *anthropic.MessageNewParams) bool {
	effort := p.effort
	if budgetThinkingModel(string(p.model)) {
		params.OutputConfig.Effort = anthropic.OutputConfigEffort(effort)
		budget := resolveThinkingBudget(ctx, p.MaxTokens)
		if budget > 0 {
			params.Thinking = anthropic.ThinkingConfigParamOfEnabled(budget)
		}
		return budget > 0
	}
	budget := agent.ThinkingBudgetFromContext(ctx)
	if budget > 0 {
		params.Thinking = anthropic.ThinkingConfigParamUnion{OfAdaptive: &anthropic.ThinkingConfigAdaptiveParam{}}
		if effort == "" {
			effort = effortForBudget(budget)
		}
	}
	params.OutputConfig.Effort = anthropic.OutputConfigEffort(effort)
	return budget > 0
}

// budgetThinkingPrefixes lists the model families released before adaptive
// thinking. The set is closed: every later model takes adaptive thinking, so
// a new model never needs an entry here.
var budgetThinkingPrefixes = []string{
	"claude-3",
	"claude-opus-4-0", "claude-opus-4-1", "claude-opus-4-5", "claude-opus-4-2025",
	"claude-sonnet-4-0", "claude-sonnet-4-5", "claude-sonnet-4-2025",
	"claude-haiku-4-5",
}

// budgetThinkingModel reports whether model only accepts thinking as
// {type: "enabled", budget_tokens: N}.
func budgetThinkingModel(model string) bool {
	for _, prefix := range budgetThinkingPrefixes {
		if strings.HasPrefix(model, prefix) {
			return true
		}
	}
	return false
}

// effortForBudget maps a thinking budget to an effort level, with the same
// thresholds the OpenAI adapter uses for reasoning_effort.
func effortForBudget(budget int) string {
	switch {
	case budget <= 2048:
		return "low"
	case budget <= 8192:
		return "medium"
	default:
		return "high"
	}
}

// resolveThinkingBudget clamps a caller-supplied extended-thinking budget
// to what the Anthropic API accepts for the current request.
//
// Returns 0 (disabled) when the context carries no budget or when maxTokens
// is too small to fit the 1024-token floor alongside any output. Otherwise
// the budget is raised to at least 1024 and lowered to at most maxTokens-1
// so the API call does not 400 on "budget_tokens must be < max_tokens".
func resolveThinkingBudget(ctx context.Context, maxTokens int64) int64 {
	b := int64(agent.ThinkingBudgetFromContext(ctx))
	if b <= 0 {
		return 0
	}
	const minBudget int64 = 1024
	if maxTokens <= minBudget {
		return 0
	}
	if b < minBudget {
		b = minBudget
	}
	if b >= maxTokens {
		b = maxTokens - 1
	}
	return b
}

// stampCacheControl marks a ContentBlockParamUnion as an Anthropic
// prompt-cache breakpoint. The request will cache every content block from
// the start of the prompt up to and including this one for ~5 minutes;
// subsequent requests that reuse the same prefix hit the cache at ~10% of
// the normal input-token cost. A request can carry up to 4 breakpoints.
func stampCacheControl(block *anthropic.ContentBlockParamUnion) {
	cc := anthropic.NewCacheControlEphemeralParam()
	switch {
	case block.OfText != nil:
		block.OfText.CacheControl = cc
	case block.OfToolUse != nil:
		block.OfToolUse.CacheControl = cc
	case block.OfToolResult != nil:
		block.OfToolResult.CacheControl = cc
	case block.OfImage != nil:
		block.OfImage.CacheControl = cc
	case block.OfDocument != nil:
		block.OfDocument.CacheControl = cc
	}
}

// blocksFromMediaParts converts MediaParts into the Anthropic
// ContentBlockParamUnion sequence. A non-empty caption is prepended as a
// text block so plain prompts like "What's in this image?" ride along with
// the image.
//
// Raw bytes are base64-encoded; URLs pass through as URLImageSourceParam.
//
// A part this adapter cannot render fails the whole message with
// agent.ErrUnrenderablePart. Skipping it was the earlier behavior and was
// wrong: Anthropic accepts no audio or video block at all, so a caller who
// sent one got back a confident answer from a model that received only the
// caption, indistinguishable from success. See that error's doc. Empty text
// parts remain skipped — they carry nothing to lose.
func blocksFromMediaParts(caption string, parts []history.MediaPart) ([]anthropic.ContentBlockParamUnion, error) {
	blocks := make([]anthropic.ContentBlockParamUnion, 0, len(parts)+1)
	if caption != "" {
		blocks = append(blocks, anthropic.NewTextBlock(caption))
	}
	for i, p := range parts {
		switch p.Type {
		case history.PartText:
			if p.Text == "" {
				continue
			}
			blocks = append(blocks, anthropic.NewTextBlock(p.Text))
		case history.PartImage:
			switch {
			case len(p.Data) > 0:
				mime := p.MIME
				if mime == "" {
					mime = "image/png"
				}
				blocks = append(blocks, anthropic.NewImageBlockBase64(mime, base64.StdEncoding.EncodeToString(p.Data)))
			case p.URL != "":
				blocks = append(blocks, anthropic.NewImageBlock(anthropic.URLImageSourceParam{URL: p.URL}))
			default:
				return nil, fmt.Errorf("anthropic: %w: part %d is an image with neither URL nor Data", agent.ErrUnrenderablePart, i)
			}
		default:
			return nil, fmt.Errorf("anthropic: %w: part %d has unsupported type %q", agent.ErrUnrenderablePart, i, p.Type)
		}
	}
	if len(blocks) == 0 {
		return nil, fmt.Errorf("anthropic: %w: %d parts produced no renderable content", agent.ErrUnrenderablePart, len(parts))
	}
	return blocks, nil
}
