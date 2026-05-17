package tools

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Selector performs dynamic tool selection: given a natural-language query it
// returns the top-K tools from a Registry ranked by cosine similarity between
// the query embedding and each tool's pre-computed description embedding.
//
// A selector is built once per Registry; tool embeddings are computed eagerly
// on construction so request-path cost is one Embed(query) + O(N) cosine.
//
// Register new tools on the underlying Registry after construction by calling
// Refresh(ctx) to re-embed.
type Selector struct {
	embedder Embedder
	topK     int
	pinned   map[string]struct{}

	mu         sync.RWMutex
	registry   *Registry
	toolNames  []string
	toolEmbeds [][]float32
}

// SelectorOption configures a Selector at construction time.
type SelectorOption func(*Selector)

// WithPinned marks tool names that must always be included in Select results,
// regardless of similarity score. Useful for tools the agent should never be
// denied (e.g. "ask_user", "final_answer").
func WithPinned(names ...string) SelectorOption {
	return func(s *Selector) {
		for _, n := range names {
			s.pinned[n] = struct{}{}
		}
	}
}

// NewSelector builds a Selector over registry with the given embedder. topK is
// the number of tools to return from Select (pinned tools are added on top and
// do not count toward the limit). Embeds every tool description immediately.
//
// topK <= 0 disables ranking: Select returns all tools (useful as a test hook
// or to opt out at runtime without nil-checking).
func NewSelector(ctx context.Context, registry *Registry, embedder Embedder, topK int, opts ...SelectorOption) (*Selector, error) {
	if registry == nil {
		return nil, fmt.Errorf("tools: NewSelector: registry is nil")
	}
	if embedder == nil {
		return nil, fmt.Errorf("tools: NewSelector: embedder is nil")
	}
	s := &Selector{
		embedder: embedder,
		topK:     topK,
		pinned:   make(map[string]struct{}),
		registry: registry,
	}
	for _, opt := range opts {
		opt(s)
	}
	if err := s.Refresh(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// Refresh re-embeds all tool descriptions currently in the registry. Call
// after Register-ing new tools on the underlying registry.
func (s *Selector) Refresh(ctx context.Context) error {
	all := s.registry.All()
	if len(all) == 0 {
		s.mu.Lock()
		s.toolNames = nil
		s.toolEmbeds = nil
		s.mu.Unlock()
		return nil
	}
	names := make([]string, len(all))
	texts := make([]string, len(all))
	for i, t := range all {
		desc := t.Descriptor()
		names[i] = desc.Name
		texts[i] = toolEmbedText(desc)
	}
	embeds, err := s.embedder.Embed(ctx, texts)
	if err != nil {
		return fmt.Errorf("tools: Selector.Refresh: embed tools: %w", err)
	}
	if len(embeds) != len(texts) {
		return fmt.Errorf("tools: Selector.Refresh: embedder returned %d vectors for %d inputs", len(embeds), len(texts))
	}
	s.mu.Lock()
	s.toolNames = names
	s.toolEmbeds = embeds
	s.mu.Unlock()
	return nil
}

// Select returns the top-K tools most relevant to query, plus any pinned
// tools. If query is empty or topK <= 0 the full tool list is returned. The
// result preserves Registry's alphabetical ordering for determinism.
//
// Never returns an error solely because a tool was added/removed after
// construction — tools not in the embedding set are simply skipped. Call
// Refresh to pick them up for future Select calls.
func (s *Selector) Select(ctx context.Context, query string) ([]Tool, error) {
	s.mu.RLock()
	names := s.toolNames
	embeds := s.toolEmbeds
	topK := s.topK
	s.mu.RUnlock()

	if len(names) == 0 {
		return nil, nil
	}
	query = strings.TrimSpace(query)
	if query == "" || topK <= 0 || topK >= len(names) {
		return s.registry.All(), nil
	}

	qvec, err := s.embedder.Embed(ctx, []string{query})
	if err != nil {
		return nil, fmt.Errorf("tools: Selector.Select: embed query: %w", err)
	}
	if len(qvec) != 1 || len(qvec[0]) == 0 {
		return nil, fmt.Errorf("tools: Selector.Select: embedder returned no query vector")
	}

	type scored struct {
		name  string
		score float64
	}
	scoreds := make([]scored, len(names))
	for i, name := range names {
		scoreds[i] = scored{name: name, score: cosineSimilarity(qvec[0], embeds[i])}
	}
	sort.SliceStable(scoreds, func(i, j int) bool { return scoreds[i].score > scoreds[j].score })

	selected := make(map[string]struct{}, topK+len(s.pinned))
	for i := 0; i < topK && i < len(scoreds); i++ {
		selected[scoreds[i].name] = struct{}{}
	}
	for name := range s.pinned {
		selected[name] = struct{}{}
	}

	orderedNames := make([]string, 0, len(selected))
	for name := range selected {
		orderedNames = append(orderedNames, name)
	}
	sort.Strings(orderedNames)

	out := make([]Tool, 0, len(orderedNames))
	for _, name := range orderedNames {
		if t, ok := s.registry.Get(name); ok {
			out = append(out, t)
		}
	}
	return out, nil
}

// SelectRegistry is a convenience that returns a fresh Registry populated with
// the tools Select would return. The returned Registry is safe to pass to an
// LLMProvider that expects *tools.Registry (e.g. AgentLoop's LLM field).
func (s *Selector) SelectRegistry(ctx context.Context, query string) (*Registry, error) {
	picked, err := s.Select(ctx, query)
	if err != nil {
		return nil, err
	}
	out := NewRegistry()
	for _, t := range picked {
		out.Register(t)
	}
	return out, nil
}

// toolEmbedText builds the text that represents a tool for embedding. Name,
// description, and parameter names all carry signal — joining them gives the
// embedder more lexical surface than description alone.
func toolEmbedText(desc ToolDescriptor) string {
	var b strings.Builder
	b.WriteString(desc.Name)
	b.WriteString(": ")
	b.WriteString(desc.Description)
	schema := desc.Parameters
	if len(schema.Properties) > 0 {
		b.WriteString(" | params: ")
		keys := make([]string, 0, len(schema.Properties))
		for k := range schema.Properties {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteString(strings.Join(keys, ", "))
	}
	return b.String()
}
