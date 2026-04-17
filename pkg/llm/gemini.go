// Package llm provides LLM provider implementations.
package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/hung12ct/gopheragent/pkg/agent"
	"github.com/hung12ct/gopheragent/pkg/history"
	"github.com/hung12ct/gopheragent/pkg/tools"
	"google.golang.org/genai"
)

// GeminiProvider implements agent.LLMProvider using Google Gemini native SDK.
type GeminiProvider struct {
	client *genai.Client
	model  string
}

// NewGeminiProvider creates a wrapper over Google Gemini's API.
func NewGeminiProvider(apiKey string, model string) (*GeminiProvider, error) {
	if apiKey == "" {
		apiKey = os.Getenv("GEMINI_API_KEY")
	}
	if apiKey == "" {
		return nil, errors.New("GEMINI_API_KEY is not set in environment")
	}
	if model == "" {
		model = "gemini-2.5-flash"
	}

	client, err := genai.NewClient(context.Background(), &genai.ClientConfig{
		APIKey: apiKey,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create gemini client: %w", err)
	}

	return &GeminiProvider{
		client: client,
		model:  model,
	}, nil
}

// GenerateStream maps GopherAgent history to Gemini API format and triggers streaming generation.
func (p *GeminiProvider) GenerateStream(ctx context.Context, memory []history.Message, availableTools *tools.Registry, streamChan chan<- agent.StreamEvent) (agent.LLMResult, error) {
	var contents []*genai.Content
	var systemInstruction *genai.Content

	for _, m := range memory {
		if m.Role == "system" {
			systemInstruction = &genai.Content{
				Parts: []*genai.Part{{Text: m.Content}},
			}
			continue
		}

		var role string
		var parts []*genai.Part

		switch m.Role {
		case "user":
			role = "user"
			if len(m.Parts) > 0 {
				parts = append(parts, geminiPartsFromMediaParts(m.Content, m.Parts)...)
			} else {
				parts = append(parts, &genai.Part{Text: m.Content})
			}
		case "assistant":
			role = "model"
			if m.Content != "" {
				parts = append(parts, &genai.Part{Text: m.Content})
			}
			for _, tc := range m.ToolCalls {
				var argsMap map[string]any
				if tc.Arguments != "" {
					_ = json.Unmarshal([]byte(tc.Arguments), &argsMap)
				}
				parts = append(parts, &genai.Part{
					FunctionCall: &genai.FunctionCall{
						Name: tc.Name,
						Args: argsMap,
					},
				})
			}
		case "tool":
			role = "user" // Tool responses are sent as 'user' role with FunctionResponse part
			var resultObj map[string]any
			// Try parsing as JSON so the tool response is structured, fallback to wrapped text
			if err := json.Unmarshal([]byte(m.Content), &resultObj); err != nil {
				resultObj = map[string]any{"result": m.Content}
			}
			parts = append(parts, &genai.Part{
				FunctionResponse: &genai.FunctionResponse{
					Name:     m.ToolCallID, // For gemini we need to match the func name here. We rely on ToolCallID housing the function name in this simple mapping or use a default.
					Response: resultObj,
				},
			})
		}
		if role != "" && len(parts) > 0 {
			contents = append(contents, &genai.Content{
				Role:  role,
				Parts: parts,
			})
		}
	}

	config := &genai.GenerateContentConfig{
		SystemInstruction: systemInstruction,
	}

	if availableTools != nil {
		var geminiTools []*genai.Tool
		var funcDecls []*genai.FunctionDeclaration
		for _, t := range availableTools.GetAll() {
			schema := t.ParametersSchema()
			funcDecls = append(funcDecls, &genai.FunctionDeclaration{
				Name:        t.Name(),
				Description: t.Description(),
				Parameters:  mapRegistrySchemaToGeminiSchema(schema),
			})
		}
		if len(funcDecls) > 0 {
			geminiTools = append(geminiTools, &genai.Tool{
				FunctionDeclarations: funcDecls,
			})
			config.Tools = geminiTools
		}
	}

	iter := p.client.Models.GenerateContentStream(ctx, p.model, contents, config)
	
	streamChan <- agent.StreamEvent{Type: "thought", Content: "Analyzing with Gemini..."}

	var finalContent string
	var pendingCalls []agent.PendingToolCall
	var usage agent.TokenUsage

	for resp, err := range iter {
		if err != nil {
			return agent.LLMResult{}, err
		}

		if resp == nil {
			continue
		}
		if resp.UsageMetadata != nil {
			usage = agent.TokenUsage{
				PromptTokens:     int(resp.UsageMetadata.PromptTokenCount),
				CompletionTokens: int(resp.UsageMetadata.CandidatesTokenCount),
				TotalTokens:      int(resp.UsageMetadata.TotalTokenCount),
			}
		}
		if len(resp.Candidates) == 0 || resp.Candidates[0].Content == nil {
			continue
		}

		for _, part := range resp.Candidates[0].Content.Parts {
			if part.Text != "" {
				finalContent += part.Text
				streamChan <- agent.StreamEvent{Type: "content", Content: part.Text}
			}
			if part.FunctionCall != nil {
				argsBytes, _ := json.Marshal(part.FunctionCall.Args)
				pendingCalls = append(pendingCalls, agent.PendingToolCall{
					ID:       part.FunctionCall.Name, // Gemini doesn't use explicit call IDs, using name as ID is a common fallback
					Name:     part.FunctionCall.Name,
					ArgsJSON: string(argsBytes),
				})
			}
		}
	}

	return agent.LLMResult{
		Content:   finalContent,
		ToolCalls: pendingCalls,
		Usage:     usage,
	}, nil
}

// geminiPartsFromMediaParts converts MediaParts into Gemini's native Part
// slice. Inline raw bytes become InlineData blobs; URLs become FileData
// entries (Gemini Files API / Cloud Storage URIs, or plain https for
// gemini-2.5-flash and newer).
//
// A non-empty caption is prepended as a text part so prompts travel
// alongside the media. MIME defaults to image/png when unspecified.
func geminiPartsFromMediaParts(caption string, parts []history.MediaPart) []*genai.Part {
	out := make([]*genai.Part, 0, len(parts)+1)
	if caption != "" {
		out = append(out, &genai.Part{Text: caption})
	}
	for _, p := range parts {
		switch p.Type {
		case history.PartText:
			if p.Text == "" {
				continue
			}
			out = append(out, &genai.Part{Text: p.Text})
		case history.PartImage:
			mime := p.MIME
			if mime == "" {
				mime = "image/png"
			}
			if len(p.Data) > 0 {
				out = append(out, &genai.Part{
					InlineData: &genai.Blob{MIMEType: mime, Data: p.Data},
				})
			} else if p.URL != "" {
				out = append(out, &genai.Part{
					FileData: &genai.FileData{FileURI: p.URL, MIMEType: mime},
				})
			}
		}
	}
	return out
}

func mapRegistrySchemaToGeminiSchema(s tools.ToolSchema) *genai.Schema {
	if s.Type == "" {
		return nil
	}
	
	geminiType := genai.TypeObject
	switch s.Type {
	case "string":
		geminiType = genai.TypeString
	case "integer":
		geminiType = genai.TypeInteger
	case "number":
		geminiType = genai.TypeNumber
	case "boolean":
		geminiType = genai.TypeBoolean
	case "array":
		geminiType = genai.TypeArray
	case "object":
		geminiType = genai.TypeObject
	}

	schema := &genai.Schema{
		Type:        geminiType,
	}

	if len(s.Required) > 0 {
		schema.Required = s.Required
	}

	if s.Properties != nil {
		schema.Properties = make(map[string]*genai.Schema)
		for k, v := range s.Properties {
			vMap, ok := v.(map[string]any)
			if !ok {
				continue
			}
			
			childSchema := tools.ToolSchema{} // fallback parsing
			if t, ok := vMap["type"].(string); ok {
				childSchema.Type = t
			}
			
			childGeminiSchema := mapRegistrySchemaToGeminiSchema(childSchema)
			if childGeminiSchema != nil {
				if d, ok := vMap["description"].(string); ok {
					childGeminiSchema.Description = d
				}
				schema.Properties[k] = childGeminiSchema
			}
		}
	}

	return schema
}
