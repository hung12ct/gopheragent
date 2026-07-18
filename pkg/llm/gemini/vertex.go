package gemini

import (
	"context"
	"errors"
	"fmt"
	"os"

	"google.golang.org/genai"
)

// NewVertex returns a Provider wired to Vertex AI instead
// of the public Gemini API. Vertex uses Application Default Credentials (ADC)
// plus a GCP project/location rather than an API key — run `gcloud auth
// application-default login` or mount a service-account key via
// GOOGLE_APPLICATION_CREDENTIALS.
//
// Empty projectID / location fall back to GOOGLE_CLOUD_PROJECT and
// GOOGLE_CLOUD_LOCATION respectively. Empty model defaults to
// "gemini-2.5-flash".
//
// The returned *Provider reuses the same GenerateStream implementation
// as the public-API backend — only the transport differs.
func NewVertex(projectID, location, model string, opts ...Option) (*Provider, error) {
	if projectID == "" {
		projectID = os.Getenv("GOOGLE_CLOUD_PROJECT")
	}
	if projectID == "" {
		return nil, errors.New("gemini: GOOGLE_CLOUD_PROJECT is not set and projectID argument is empty")
	}
	if location == "" {
		location = os.Getenv("GOOGLE_CLOUD_LOCATION")
	}
	if location == "" {
		location = "us-central1"
	}
	if model == "" {
		model = "gemini-2.5-flash"
	}

	client, err := genai.NewClient(context.Background(), &genai.ClientConfig{
		Backend:  genai.BackendVertexAI,
		Project:  projectID,
		Location: location,
	})
	if err != nil {
		return nil, fmt.Errorf("gemini: failed to create vertex gemini client: %w", err)
	}

	p := &Provider{
		client: client,
		model:  model,
	}
	for _, opt := range opts {
		opt(p)
	}
	return p, nil
}
