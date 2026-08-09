package openai

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/sashabaranov/go-openai"
)

// clientConfig carries the transport settings every client in this package
// shares: which endpoint to talk to and what headers to add.
type clientConfig struct {
	baseURL string
	headers map[string]string
}

// Option configures a chat Provider. Both the sampling options
// (WithTemperature, WithTopP, WithSeed) and the transport options
// (WithBaseURL, WithHTTPHeader) satisfy it, so one call can mix them.
type Option interface{ applyProvider(*Provider) }

// ClientOption configures transport for any client in this package —
// Provider, Embedder, VisionAnalyzer, SummaryProvider.
//
// It is a strict subset of Option. Sampling has no meaning for an
// embedder, so NewEmbedder and friends accept only these: passing
// WithTemperature to one is a compile error rather than an option that
// silently does nothing.
type ClientOption interface {
	Option
	applyClient(*clientConfig)
}

// providerOptionFunc adapts a plain func to Option, for settings that
// exist only on a chat Provider.
type providerOptionFunc func(*Provider)

func (f providerOptionFunc) applyProvider(p *Provider) { f(p) }

// clientOptionFunc adapts a plain func to ClientOption. applyProvider
// routes through the Provider's embedded clientConfig, which is what
// lets a single transport option work on every constructor.
type clientOptionFunc func(*clientConfig)

func (f clientOptionFunc) applyClient(c *clientConfig) { f(c) }
func (f clientOptionFunc) applyProvider(p *Provider)   { f(&p.cfg) }

// WithBaseURL points the client at an OpenAI-compatible endpoint —
// OpenRouter, Ollama, vLLM, Groq, Together, or any gateway speaking the
// same API.
//
// This is the only way to keep an Embedder, VisionAnalyzer, or
// SummaryProvider off api.openai.com. Without it they call OpenAI
// directly, which on a deliberately local or gateway-only deployment
// means an unexpected egress and a key the operator may not hold.
//
// url must be absolute, HTTP(S), and free of embedded credentials, a
// query, or a fragment; it is validated at construction. Prefer HTTPS
// except for local development endpoints.
func WithBaseURL(url string) ClientOption {
	return clientOptionFunc(func(c *clientConfig) { c.baseURL = url })
}

// WithHTTPHeader adds a header to every request the client makes. It is
// useful for compatible gateways that accept attribution or routing
// headers.
//
// Headers are applied after the SDK has built the request, so a name the
// SDK already sets is overwritten: passing "Authorization" replaces the
// bearer token derived from apiKey. Repeated calls with the same name
// keep the last value.
func WithHTTPHeader(name, value string) ClientOption {
	return clientOptionFunc(func(c *clientConfig) {
		if c.headers == nil {
			c.headers = make(map[string]string)
		}
		c.headers[name] = value
	})
}

// validateBaseURL normalizes and checks a compatible-endpoint URL,
// returning the trimmed form. Trailing slashes are stripped because the
// SDK appends its own path segment.
//
// Errors are returned unprefixed; each constructor adds its own
// "openai: <Ctor>: " so the message names the call the caller actually
// made instead of stacking the package prefix twice.
func validateBaseURL(raw string) (string, error) {
	trimmed := strings.TrimRight(strings.TrimSpace(raw), "/")
	if trimmed == "" {
		return "", errors.New("baseURL is required")
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("parsing baseURL: %w", err)
	}
	if u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") ||
		u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("baseURL must be an absolute HTTP(S) URL without credentials, query, or fragment")
	}
	return trimmed, nil
}

// newClient builds a go-openai client for apiKey under these transport
// settings. Every constructor in the package routes through here so that
// baseURL validation and header injection cannot drift between them.
func (c clientConfig) newClient(apiKey string) (*openai.Client, error) {
	config := openai.DefaultConfig(apiKey)
	if c.baseURL != "" {
		validated, err := validateBaseURL(c.baseURL)
		if err != nil {
			return nil, err
		}
		config.BaseURL = validated
	}
	if len(c.headers) > 0 {
		config.HTTPClient = headerDoer{base: config.HTTPClient, headers: c.headers}
	}
	return openai.NewClientWithConfig(config), nil
}

// newClientFor resolves apiKey (falling back to OPENAI_API_KEY), applies
// opts, and builds the client. Shared by the non-chat constructors.
func newClientFor(apiKey, ctor string, opts []ClientOption) (*openai.Client, error) {
	apiKey = resolveAPIKey(apiKey)
	if apiKey == "" {
		return nil, fmt.Errorf("openai: %s: API key is not set", ctor)
	}
	var cfg clientConfig
	for _, opt := range opts {
		opt.applyClient(&cfg)
	}
	client, err := cfg.newClient(apiKey)
	if err != nil {
		return nil, fmt.Errorf("openai: %s: %w", ctor, err)
	}
	return client, nil
}

type headerDoer struct {
	base    openai.HTTPDoer
	headers map[string]string
}

// Do copies the request shallowly and clones only its header, the standard
// RoundTripper idiom: req.Clone would deep-copy the header a second time and
// throw the first copy away on every call.
func (d headerDoer) Do(req *http.Request) (*http.Response, error) {
	clone := *req
	clone.Header = req.Header.Clone()
	for name, value := range d.headers {
		clone.Header.Set(name, value)
	}
	return d.base.Do(&clone)
}
