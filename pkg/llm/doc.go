// Package llm hosts provider-neutral LLM utilities — currently the
// rule-based RouterProvider for multi-model selection — and the per-provider
// subpackages.
//
// Concrete providers live in their own subpackages so a binary that uses one
// vendor does not statically link the other vendors' SDKs:
//
//   - pkg/llm/anthropic — Claude (Messages API)
//   - pkg/llm/openai    — OpenAI + OpenAI-compatible endpoints, embedder,
//     vision analyzer, summary provider
//   - pkg/llm/gemini    — Google Gemini (public API + Vertex AI), embedder,
//     media analyzer
package llm
