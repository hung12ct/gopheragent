package openai

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/hung12ct/gopheragent/pkg/audio"
	"github.com/sashabaranov/go-openai"
)

// maxClipBytes is the OpenAI audio endpoint's documented per-request limit
// (25 MB). Checked before upload so an oversized clip fails immediately
// instead of after transferring the payload.
const maxClipBytes = 25 << 20

// Transcriber implements audio.Transcriber against OpenAI's audio
// transcription endpoint.
//
// Model choice changes the shape of the result. whisper-1 supports the
// verbose_json response format and so populates Transcript.Segments,
// Language, and Duration. The gpt-4o-transcribe family accepts only json and
// text, so it returns text alone with Segments nil — correct, but useless to
// a caller that needs timestamps. whisper-1 is the default for that reason.
//
// Pass WithBaseURL to transcribe against an OpenAI-compatible endpoint (a
// self-hosted Whisper server, Groq) instead of api.openai.com.
type Transcriber struct {
	client *openai.Client
	model  string
}

var _ audio.Transcriber = (*Transcriber)(nil)

// NewTranscriber constructs a transcriber. apiKey falls back to
// OPENAI_API_KEY. model defaults to whisper-1.
func NewTranscriber(apiKey string, model string, opts ...ClientOption) (*Transcriber, error) {
	client, err := newClientFor(apiKey, "NewTranscriber", opts)
	if err != nil {
		return nil, err
	}
	if model == "" {
		model = openai.Whisper1
	}
	return &Transcriber{client: client, model: model}, nil
}

// Transcribe converts one clip to text.
func (t *Transcriber) Transcribe(ctx context.Context, clip audio.Clip, opts audio.Options) (audio.Transcript, error) {
	if err := clip.Validate(); err != nil {
		return audio.Transcript{}, fmt.Errorf("openai: Transcriber: %w", err)
	}
	if len(clip.Data) > maxClipBytes {
		return audio.Transcript{}, fmt.Errorf("openai: Transcriber: %w: %d bytes exceeds %d",
			audio.ErrTooLarge, len(clip.Data), maxClipBytes)
	}

	format := openai.AudioResponseFormatJSON
	var granularity []openai.TranscriptionTimestampGranularity
	if t.supportsSegments() {
		format = openai.AudioResponseFormatVerboseJSON
		granularity = []openai.TranscriptionTimestampGranularity{
			openai.TranscriptionTimestampGranularitySegment,
		}
	}

	// With Reader set, FilePath is a filename hint for the multipart form
	// rather than a path on disk. The endpoint infers the container from its
	// extension, so a wrong extension rejects a clip that would decode fine.
	resp, err := t.client.CreateTranscription(ctx, openai.AudioRequest{
		Model:                  t.model,
		FilePath:               "clip." + audio.Ext(clip.MIME),
		Reader:                 bytes.NewReader(clip.Data),
		Language:               opts.Language,
		Prompt:                 opts.Prompt,
		Format:                 format,
		TimestampGranularities: granularity,
	})
	if err != nil {
		return audio.Transcript{}, fmt.Errorf("openai: Transcriber: transcribe: %w", classifyErr(err))
	}

	out := audio.Transcript{
		Text:     strings.TrimSpace(resp.Text),
		Language: resp.Language,
		Duration: secondsToDuration(resp.Duration),
	}
	if len(resp.Segments) > 0 {
		out.Segments = make([]audio.Segment, 0, len(resp.Segments))
		for _, s := range resp.Segments {
			out.Segments = append(out.Segments, audio.Segment{
				Start: secondsToDuration(s.Start),
				End:   secondsToDuration(s.End),
				Text:  strings.TrimSpace(s.Text),
			})
		}
	}
	return out, nil
}

// supportsSegments reports whether the configured model accepts the
// verbose_json response format. Only the whisper family does; asking for it
// elsewhere fails the request outright rather than degrading, so this must
// stay conservative in the other direction — a missed match costs timings,
// a false match costs the whole request.
//
// Matched as a substring rather than a prefix because compatible endpoints
// name the same weights differently: "Systran/faster-whisper-large-v3" and
// "ggml-whisper.cpp" both speak verbose_json.
func (t *Transcriber) supportsSegments() bool {
	return strings.Contains(strings.ToLower(t.model), "whisper")
}

// secondsToDuration converts the endpoint's fractional seconds to a Duration
// without losing sub-second precision to integer truncation.
func secondsToDuration(sec float64) time.Duration {
	return time.Duration(sec * float64(time.Second))
}
