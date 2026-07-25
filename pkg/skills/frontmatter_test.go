package skills

import "testing"

func TestParseSkillDoc_HappyPath(t *testing.T) {
	doc := `---
name: pdf-reader
description: Extract text from PDFs.
license: MIT
compatibility: needs python3
metadata:
  author: hung
allowed-tools: Bash(git:*) Read
---
# PDF Reader

Body text.
`
	fm, body, lenient, err := parseSkillDoc(doc, true)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if lenient {
		t.Fatal("valid YAML should not take the lenient path")
	}
	if fm.Name != "pdf-reader" || fm.Description != "Extract text from PDFs." {
		t.Fatalf("fields not parsed: %+v", fm)
	}
	if fm.License != "MIT" || fm.Compatibility != "needs python3" {
		t.Fatalf("optional fields lost: %+v", fm)
	}
	if fm.Metadata["author"] != "hung" {
		t.Fatalf("metadata lost: %+v", fm.Metadata)
	}
	if fm.AllowedTools != "Bash(git:*) Read" {
		t.Fatalf("allowed-tools lost: %q", fm.AllowedTools)
	}
	if body != "# PDF Reader\n\nBody text.\n" {
		t.Fatalf("body not stripped cleanly: %q", body)
	}
}

func TestParseSkillDoc_StrictRejectsUnknownKey(t *testing.T) {
	doc := `---
name: thing
description: A thing.
totally-unknown: surprise
---
Body.
`
	if _, _, _, err := parseSkillDoc(doc, true); err == nil {
		t.Fatal("strict mode must reject an unknown frontmatter key")
	}
	// Non-strict must accept the same document.
	fm, _, _, err := parseSkillDoc(doc, false)
	if err != nil {
		t.Fatalf("non-strict should accept unknown keys: %v", err)
	}
	if fm.Name != "thing" {
		t.Fatalf("non-strict lost fields: %+v", fm)
	}
}

func TestParseSkillDoc_UnquotedColonFallsBackToLenient(t *testing.T) {
	// The spec's own idiom. This is not valid YAML.
	doc := `---
name: research
description: Use when: the user asks for citations
---
Body.
`
	fm, body, lenient, err := parseSkillDoc(doc, true)
	if err != nil {
		t.Fatalf("lenient fallback should rescue this: %v", err)
	}
	if !lenient {
		t.Fatal("expected the lenient path to be taken")
	}
	if fm.Name != "research" {
		t.Fatalf("name not recovered: %+v", fm)
	}
	if fm.Description != "Use when: the user asks for citations" {
		t.Fatalf("description truncated at the colon: %q", fm.Description)
	}
	if body != "Body.\n" {
		t.Fatalf("body wrong: %q", body)
	}
}

// The security decision: a block whose YAML did not parse must not yield a
// privilege grant or arbitrary metadata read out of half-understood text.
func TestParseSkillDoc_LenientFallbackDropsPrivilegedFields(t *testing.T) {
	doc := `---
name: sneaky
description: Use when: anything
allowed-tools: Bash(rm:*) Write
license: proprietary
compatibility: any
metadata:
  owner: attacker
---
Body.
`
	fm, _, lenient, err := parseSkillDoc(doc, true)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !lenient {
		t.Fatal("expected the lenient path")
	}
	if fm.AllowedTools != "" {
		t.Fatalf("lenient fallback must not recover allowed-tools, got %q", fm.AllowedTools)
	}
	if fm.License != "" || fm.Compatibility != "" {
		t.Fatalf("lenient fallback must not recover optional fields: %+v", fm)
	}
	if len(fm.Metadata) != 0 {
		t.Fatalf("lenient fallback must not recover metadata: %+v", fm.Metadata)
	}
}

func TestParseSkillDoc_FenceErrors(t *testing.T) {
	tests := []struct {
		name string
		doc  string
	}{
		{"no fence at all", "# Just markdown\n"},
		{"fence not on line 1", "\n---\nname: x\ndescription: y\n---\n"},
		{"no closing fence", "---\nname: x\ndescription: y\n"},
		{"horizontal rule only", "Some text\n\n---\n\nMore text\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, _, _, err := parseSkillDoc(tt.doc, true); err == nil {
				t.Fatal("expected an error, got nil")
			}
		})
	}
}

func TestParseSkillDoc_UnparseableFrontmatterRejected(t *testing.T) {
	// Fenced, but neither strict YAML nor the scanner can find both fields.
	doc := "---\n\t\t: : :\n---\nBody.\n"
	if _, _, _, err := parseSkillDoc(doc, true); err == nil {
		t.Fatal("expected rejection when name and description are unrecoverable")
	}
}

func TestParseSkillDoc_BOMAndCRLF(t *testing.T) {
	doc := "\ufeff---\r\nname: crlf\r\ndescription: Handles CRLF.\r\n---\r\nBody.\r\n"
	fm, body, _, err := parseSkillDoc(doc, true)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if fm.Name != "crlf" || fm.Description != "Handles CRLF." {
		t.Fatalf("BOM/CRLF broke parsing: %+v", fm)
	}
	if body != "Body.\r\n" {
		t.Fatalf("body wrong: %q", body)
	}
}

func TestParseSkillDoc_QuotedValues(t *testing.T) {
	doc := "---\nname: \"quoted\"\ndescription: 'Single quoted: with colon'\n---\nBody.\n"
	fm, _, _, err := parseSkillDoc(doc, true)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if fm.Name != "quoted" {
		t.Fatalf("quotes not stripped from name: %q", fm.Name)
	}
	if fm.Description != "Single quoted: with colon" {
		t.Fatalf("quoted description wrong: %q", fm.Description)
	}
}

func TestValidateName(t *testing.T) {
	long := make([]byte, MaxNameLen+1)
	for i := range long {
		long[i] = 'a'
	}
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"empty", "", true},
		{"too long", string(long), true},
		{"exactly max", string(long[:MaxNameLen]), false},
		{"underscore", "foo_bar", true},
		{"space", "foo bar", true},
		{"slash", "foo/bar", true},
		{"dot", "foo.bar", true},
		{"mixed case and digit", "Foo-1", false},
		{"plain", "pdf-reader", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateName(tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateName(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
		})
	}
}

func TestFrontmatterValidate_Bounds(t *testing.T) {
	base := func() frontmatter {
		return frontmatter{Name: "ok", Description: "d"}
	}
	if err := base().validate(); err != nil {
		t.Fatalf("baseline should validate: %v", err)
	}

	fm := base()
	fm.Description = ""
	if err := fm.validate(); err == nil {
		t.Fatal("empty description must be rejected")
	}

	fm = base()
	fm.Description = string(make([]byte, MaxDescriptionLen+1))
	if err := fm.validate(); err == nil {
		t.Fatal("over-length description must be rejected")
	}

	fm = base()
	fm.Description = string(make([]byte, MaxDescriptionLen))
	if err := fm.validate(); err != nil {
		t.Fatalf("exactly-max description must be accepted: %v", err)
	}

	fm = base()
	fm.Compatibility = string(make([]byte, MaxCompatibilityLen+1))
	if err := fm.validate(); err == nil {
		t.Fatal("over-length compatibility must be rejected")
	}

	fm = base()
	fm.Metadata = make(map[string]string, MaxMetadataKeys+1)
	for i := 0; i <= MaxMetadataKeys; i++ {
		fm.Metadata[string(rune('a'+i%26))+string(rune('0'+i/26))] = "v"
	}
	if err := fm.validate(); err == nil {
		t.Fatal("too many metadata keys must be rejected")
	}

	fm = base()
	fm.Metadata = map[string]string{"k": string(make([]byte, MaxMetadataValueLen+1))}
	if err := fm.validate(); err == nil {
		t.Fatal("over-length metadata value must be rejected")
	}
}
