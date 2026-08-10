// Package audio defines provider-neutral types for speech-to-text.
//
// Transcription is deliberately kept outside the agent loop. Of the three
// LLM providers this framework ships, only Gemini accepts audio in a chat
// message today; Anthropic has no audio content block at all, and the OpenAI
// Chat Completions client cannot express one. Routing audio through a
// Transcriber first turns it into ordinary text, so every provider can drive
// an audio-fed agent — and the transcript, not the waveform, is what lands in
// session history.
//
// That last point matters for cost as much as for portability. History is
// re-sent on every LLM call in a session, so inline audio in a message would
// be re-uploaded on every subsequent turn. A one-hour meeting transcribed once
// costs one upload; the same meeting carried as message parts would be billed
// again on each turn of the conversation about it.
package audio

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Sentinel errors returned by Transcriber implementations. They are separate
// types because they demand opposite responses: an oversized clip should be
// split and retried, an unsupported format should be re-encoded, and an empty
// clip is a caller bug. Route on these with errors.Is rather than matching on
// message text.
var (
	// ErrNoAudio is returned when a clip carries no sample data.
	ErrNoAudio = errors.New("audio: clip has no data")

	// ErrUnsupportedFormat is returned when the clip's MIME type is not one
	// the backend accepts. Re-encode and retry; retrying as-is always fails.
	ErrUnsupportedFormat = errors.New("audio: unsupported format")

	// ErrTooLarge is returned when a clip exceeds the backend's per-request
	// size limit. Callers feeding a live stream should respond by cutting
	// shorter chunks, not by retrying the same clip.
	ErrTooLarge = errors.New("audio: clip exceeds provider size limit")
)

// Clip is a single piece of audio to transcribe.
//
// Data holds the whole clip in memory. That is deliberate rather than a
// missing optimization: both backends buffer the full payload anyway — the
// OpenAI client assembles its multipart body into a bytes.Buffer, and Gemini
// inline data is a []byte field — so an io.Reader here would add a streaming
// API that does not stream. Callers transcribing long recordings should cut
// them into chunks, which is what a live-capture pipeline does regardless.
type Clip struct {
	// MIME is the IANA media type, e.g. "audio/wav" or "audio/webm".
	// Parameters are allowed and ignored: browsers' MediaRecorder reports
	// "audio/webm;codecs=opus", and that value is accepted verbatim.
	MIME string

	// Data is the raw encoded clip — the bytes of a .wav or .webm file, not
	// decoded PCM samples.
	Data []byte
}

// Options tunes a single transcription request. The zero value is valid and
// asks the backend to auto-detect everything.
type Options struct {
	// Language is an ISO-639-1 hint such as "en" or "vi". Empty means
	// auto-detect. Setting it cuts latency and materially improves accuracy
	// on short clips, where there is little signal to detect from.
	Language string

	// Prompt biases decoding toward expected vocabulary — proper nouns,
	// product names, jargon, acronyms. For a chunked live stream, passing the
	// tail of the previous chunk's text carries context across the seam and
	// reduces mid-word splits.
	Prompt string
}

// Segment is a timed span of transcribed speech.
type Segment struct {
	Start time.Duration // offset from the start of the clip
	End   time.Duration
	Text  string
}

// Transcript is the result of transcribing one clip.
type Transcript struct {
	// Text is the full transcription. Always populated on success.
	Text string

	// Language is the detected or configured language, best-effort, and may
	// be empty when the backend reports none.
	//
	// The spelling is the backend's, not a normalized code: Whisper returns
	// an English name ("english"), while a backend given Options.Language
	// echoes that ISO-639-1 code back. Compare it against a fixed set at your
	// own risk; it is for display and logging.
	Language string

	// Duration is the length of the source audio, best-effort. Zero when the
	// backend does not report it.
	Duration time.Duration

	// Segments carries per-span timings when the backend provides them.
	// Nil is a normal result, not an error: some backends transcribe without
	// emitting any timing at all. Callers that need timestamps must check for
	// nil rather than assuming a populated slice.
	Segments []Segment
}

// Transcriber converts audio into text. Implementations live in the provider
// subpackages under pkg/llm.
//
// Implementations must be safe for concurrent use: a live-capture pipeline
// transcribes overlapping chunks from several goroutines to keep up with
// real time, which is the primary use for this seam.
type Transcriber interface {
	Transcribe(ctx context.Context, clip Clip, opts Options) (Transcript, error)
}

// extByMIME maps accepted media types to the file extension backends use to
// infer the container format. Both the audio/* and video/* spellings of the
// shared containers are listed: MediaRecorder emits "video/webm" for an
// audio-only recording on some browsers, and rejecting that would fail a clip
// the backend decodes fine.
var extByMIME = map[string]string{
	"audio/wav":       "wav",
	"audio/x-wav":     "wav",
	"audio/wave":      "wav",
	"audio/vnd.wave":  "wav",
	"audio/mpeg":      "mp3",
	"audio/mp3":       "mp3",
	"audio/mp4":       "m4a",
	"audio/x-m4a":     "m4a",
	"audio/webm":      "webm",
	"video/webm":      "webm",
	"video/mp4":       "mp4",
	"audio/ogg":       "ogg",
	"application/ogg": "ogg",
	"audio/opus":      "ogg",
	"audio/flac":      "flac",
	"audio/x-flac":    "flac",
}

// Ext returns the file extension for a media type, or "" when the type is not
// a recognized audio container. Parameters after ";" are stripped and the type
// is matched case-insensitively, so "AUDIO/WEBM;codecs=opus" resolves to
// "webm".
func Ext(mime string) string {
	base, _, _ := strings.Cut(mime, ";")
	return extByMIME[strings.ToLower(strings.TrimSpace(base))]
}

// Validate reports whether the clip is well-formed enough to send. It does not
// enforce per-provider size limits — those belong to the implementation that
// knows them.
func (c Clip) Validate() error {
	if len(c.Data) == 0 {
		return ErrNoAudio
	}
	// The sentinels already carry the package prefix, so wrapping adds only
	// the detail — otherwise every message reads "audio: audio: ...".
	if c.MIME == "" {
		return fmt.Errorf("%w: MIME is required", ErrUnsupportedFormat)
	}
	if Ext(c.MIME) == "" {
		return fmt.Errorf("%w: %q", ErrUnsupportedFormat, c.MIME)
	}
	return nil
}
