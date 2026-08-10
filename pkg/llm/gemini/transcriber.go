package gemini

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/hung12ct/gopheragent/pkg/audio"
	"google.golang.org/genai"
)

// maxInlineBytes caps what this transcriber will send as inline data.
// Gemini requires requests above roughly 20 MB to go through the Files API
// instead, which has upload-and-poll semantics that do not fit a
// single-request seam. Callers streaming a long recording should cut shorter
// chunks; ErrTooLarge tells them to.
const maxInlineBytes = 20 << 20

// transcribeInstruction constrains a generative model to behave like a
// transcription endpoint. Gemini will otherwise open with a preamble
// ("Here is the transcript:") or summarize instead of transcribing, and both
// corrupt a transcript that downstream code appends verbatim.
const transcribeInstruction = `You are a speech transcription engine.
Transcribe the supplied audio verbatim.
Output only the transcribed words. Do not add a preamble, commentary, translation, headings, quotation marks, or speaker labels that are not spoken aloud.
Preserve the original language; do not translate.
If the audio contains no intelligible speech, output nothing at all.`

// Transcriber implements audio.Transcriber using Gemini's multimodal API.
//
// Gemini has no dedicated transcription endpoint, so this drives the ordinary
// generation API with an inline audio blob and a constraining instruction.
// Two consequences follow, and both are visible in the returned Transcript:
// Segments is always nil because generation emits no timing, and the result
// is a model sample rather than a decoder output, so it can paraphrase under
// adversarial audio in a way a dedicated ASR model does not.
//
// It is a good fit when Gemini is already the deployment's provider and
// timestamps are not needed. Prefer the OpenAI Transcriber when segment
// timings matter.
type Transcriber struct {
	client *genai.Client
	model  string
}

var _ audio.Transcriber = (*Transcriber)(nil)

// NewTranscriber builds a transcriber. apiKey defaults to GEMINI_API_KEY;
// model defaults to "gemini-2.5-flash".
func NewTranscriber(apiKey, model string) (*Transcriber, error) {
	if apiKey == "" {
		apiKey = os.Getenv("GEMINI_API_KEY")
	}
	if apiKey == "" {
		return nil, fmt.Errorf("gemini: NewTranscriber: GEMINI_API_KEY not set")
	}
	if model == "" {
		model = "gemini-2.5-flash"
	}
	client, err := genai.NewClient(context.Background(), &genai.ClientConfig{APIKey: apiKey})
	if err != nil {
		return nil, fmt.Errorf("gemini: NewTranscriber: %w", err)
	}
	return &Transcriber{client: client, model: model}, nil
}

// Transcribe converts one clip to text. The returned Transcript never carries
// Segments; see the type doc.
func (t *Transcriber) Transcribe(ctx context.Context, clip audio.Clip, opts audio.Options) (audio.Transcript, error) {
	if err := clip.Validate(); err != nil {
		return audio.Transcript{}, fmt.Errorf("gemini: Transcriber: %w", err)
	}
	if len(clip.Data) > maxInlineBytes {
		return audio.Transcript{}, fmt.Errorf("gemini: Transcriber: %w: %d bytes exceeds %d",
			audio.ErrTooLarge, len(clip.Data), maxInlineBytes)
	}

	contents := []*genai.Content{{
		Role:  genai.RoleUser,
		Parts: []*genai.Part{{InlineData: &genai.Blob{MIMEType: clip.MIME, Data: clip.Data}}},
	}}

	// Temperature 0: transcription wants the single most likely token, not a
	// sample from the distribution.
	var temperature float32
	config := &genai.GenerateContentConfig{
		Temperature: &temperature,
		SystemInstruction: &genai.Content{
			Parts: []*genai.Part{{Text: buildInstruction(opts)}},
		},
	}

	resp, err := t.client.Models.GenerateContent(ctx, t.model, contents, config)
	if err != nil {
		return audio.Transcript{}, fmt.Errorf("gemini: Transcriber: transcribe: %w", classifyErr(err))
	}
	return transcriptFromResponse(resp, opts)
}

// transcriptFromResponse turns a generation response into a Transcript.
// Split out from Transcribe so the ordering below is testable without a
// client: the checks are order-sensitive in a way that is easy to regress.
func transcriptFromResponse(resp *genai.GenerateContentResponse, opts audio.Options) (audio.Transcript, error) {
	// Zero candidates means the request produced nothing — normally a
	// prompt-level block. That is not the same as audio containing no speech,
	// and reporting it as an empty transcript would let a blocked meeting
	// look like a silent one.
	if resp == nil || len(resp.Candidates) == 0 {
		return audio.Transcript{}, fmt.Errorf("gemini: Transcriber: no candidate returned")
	}
	// Checked before Content, not after: a candidate blocked for safety
	// arrives with a non-STOP reason and nil Content, so testing Content
	// first would report a content block as silence. A non-STOP reason also
	// means any parts below are a partial answer, not the whole one — the
	// same silent-prefix trap the streaming path guards.
	if reasonErr := finishReasonErr(resp.Candidates[0].FinishReason); reasonErr != nil {
		return audio.Transcript{}, fmt.Errorf("gemini: Transcriber: %w", reasonErr)
	}
	if resp.Candidates[0].Content == nil {
		// Stopped cleanly with nothing to say: audio that held no
		// intelligible speech. An empty transcript is the honest answer.
		return audio.Transcript{Language: opts.Language}, nil
	}

	var sb strings.Builder
	for _, p := range resp.Candidates[0].Content.Parts {
		sb.WriteString(p.Text)
	}
	return audio.Transcript{
		Text:     strings.TrimSpace(sb.String()),
		Language: opts.Language,
	}, nil
}

// buildInstruction folds the caller's hints into the system instruction.
// Gemini has no language or prompt parameter equivalent to a dedicated ASR
// endpoint, so both have to travel as text.
func buildInstruction(opts audio.Options) string {
	if opts.Language == "" && opts.Prompt == "" {
		return transcribeInstruction
	}
	var sb strings.Builder
	sb.WriteString(transcribeInstruction)
	if opts.Language != "" {
		fmt.Fprintf(&sb, "\nThe audio is expected to be in this language (ISO 639-1): %s", opts.Language)
	}
	if opts.Prompt != "" {
		fmt.Fprintf(&sb, "\nExpect these terms, spelled exactly as given: %s", opts.Prompt)
	}
	return sb.String()
}
