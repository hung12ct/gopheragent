package history

import (
	"bytes"
	"encoding/json"
	"testing"
)

// TestMessage_NoPartsSerializationIsBackwardCompat verifies that a Message
// with no Parts field produces JSON byte-identical to the pre-multimodal
// format. This is what guarantees on-disk session files written by older
// builds continue to deserialize correctly.
func TestMessage_NoPartsSerializationIsBackwardCompat(t *testing.T) {
	m := Message{Role: "user", Content: "hello"}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"role":"user","content":"hello"}`
	if string(b) != want {
		t.Fatalf("legacy shape broken\n got: %s\nwant: %s", b, want)
	}
}

func TestMessage_PartsRoundTrip(t *testing.T) {
	orig := Message{
		Role:    "user",
		Content: "describe these",
		Parts: []MediaPart{
			NewTextPart("frame 1:"),
			NewImagePartURL("image/jpeg", "https://example.com/a.jpg"),
			NewImagePartBytes("image/png", []byte{0x89, 0x50, 0x4e, 0x47}),
		},
	}
	b, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got Message
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Parts) != 3 {
		t.Fatalf("expected 3 parts, got %d", len(got.Parts))
	}
	if got.Parts[0].Type != PartText || got.Parts[0].Text != "frame 1:" {
		t.Fatalf("text part round-trip failed: %+v", got.Parts[0])
	}
	if got.Parts[1].URL != "https://example.com/a.jpg" {
		t.Fatalf("URL part round-trip failed: %+v", got.Parts[1])
	}
	if !bytes.Equal(got.Parts[2].Data, []byte{0x89, 0x50, 0x4e, 0x47}) {
		t.Fatalf("bytes part corruption: %x", got.Parts[2].Data)
	}
}

func TestMessage_ConstructorsAreSafe(t *testing.T) {
	cases := []struct {
		name string
		p    MediaPart
		want PartType
	}{
		{"text", NewTextPart("x"), PartText},
		{"image url", NewImagePartURL("image/png", "https://x"), PartImage},
		{"image bytes", NewImagePartBytes("image/png", []byte{1, 2}), PartImage},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.p.Type != tc.want {
				t.Fatalf("expected %q, got %q", tc.want, tc.p.Type)
			}
		})
	}
}
