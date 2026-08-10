package audio

import (
	"errors"
	"strings"
	"testing"
)

func TestExtStripsParametersAndCase(t *testing.T) {
	// MediaRecorder reports the codec as a parameter, and some browsers use
	// the video/* spelling for an audio-only recording. Both must resolve.
	for _, tc := range []struct{ mime, want string }{
		{"audio/webm", "webm"},
		{"audio/webm;codecs=opus", "webm"},
		{"audio/webm; codecs=opus", "webm"},
		{"video/webm;codecs=opus", "webm"},
		{"AUDIO/WEBM;CODECS=OPUS", "webm"},
		{"  audio/wav  ", "wav"},
		{"audio/mpeg", "mp3"},
		{"audio/x-m4a", "m4a"},
		{"audio/flac", "flac"},
		{"audio/opus", "ogg"},
		{"text/plain", ""},
		{"", ""},
	} {
		if got := Ext(tc.mime); got != tc.want {
			t.Fatalf("Ext(%q) = %q, want %q", tc.mime, got, tc.want)
		}
	}
}

func TestClipValidate(t *testing.T) {
	data := []byte{0x1, 0x2}
	for _, tc := range []struct {
		name string
		clip Clip
		want error
	}{
		{"ok", Clip{MIME: "audio/wav", Data: data}, nil},
		{"ok with codec parameter", Clip{MIME: "audio/webm;codecs=opus", Data: data}, nil},
		{"no data", Clip{MIME: "audio/wav"}, ErrNoAudio},
		{"no mime", Clip{Data: data}, ErrUnsupportedFormat},
		{"unknown mime", Clip{MIME: "application/pdf", Data: data}, ErrUnsupportedFormat},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.clip.Validate()
			if tc.want == nil {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("Validate() = %v, want %v", err, tc.want)
			}
			// The sentinels already carry the package prefix; wrapping must
			// add only detail, not a second "audio: ".
			if strings.Contains(err.Error(), "audio: audio:") {
				t.Fatalf("Validate() = %q, want a single package prefix", err)
			}
		})
	}
}

// The sentinels exist so a live-capture pipeline can route on them: a
// too-large clip should be re-cut, an unsupported one re-encoded. Distinct
// identities are the contract, so guard against a careless collapse into one.
func TestSentinelsAreDistinct(t *testing.T) {
	all := []error{ErrNoAudio, ErrUnsupportedFormat, ErrTooLarge}
	for i, a := range all {
		for j, b := range all {
			if i != j && errors.Is(a, b) {
				t.Fatalf("errors.Is(%v, %v) = true, want distinct sentinels", a, b)
			}
		}
	}
}
