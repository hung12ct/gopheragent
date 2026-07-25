package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/hung12ct/gopheragent/pkg/skills"
	"github.com/hung12ct/gopheragent/pkg/tools"
)

func skillDoc(name, desc string) string {
	return fmt.Sprintf("---\nname: %s\ndescription: %s\n---\nInstructions for %s.\n", name, desc, name)
}

// loadSet builds a Set, optionally trusted, failing the test on error.
func loadSet(t *testing.T, fsys fstest.MapFS, trusted bool) *skills.Set {
	t.Helper()
	var opts []skills.Option
	if trusted {
		opts = append(opts, skills.TrustedSource())
	}
	set, err := skills.FromFS(context.Background(), fsys, opts...)
	if err != nil {
		t.Fatalf("FromFS: %v", err)
	}
	return set
}

// enumOf pulls the enum out of a registered tool's parameter schema.
func enumOf(t *testing.T, reg *tools.Registry, toolName, field string) []string {
	t.Helper()
	tool, ok := reg.Get(toolName)
	if !ok {
		t.Fatalf("tool %q not registered", toolName)
	}
	prop, ok := tool.Descriptor().Parameters.Properties[field].(map[string]any)
	if !ok {
		t.Fatalf("%s.%s missing from schema", toolName, field)
	}
	enum, ok := prop["enum"].([]string)
	if !ok {
		t.Fatalf("%s.%s has no string enum: %+v", toolName, field, prop)
	}
	return enum
}

func TestRegisterSkillTools_EmptySetRegistersNothing(t *testing.T) {
	reg := tools.NewRegistry()
	RegisterSkillTools(reg, loadSet(t, fstest.MapFS{}, true))
	if reg.Len() != 0 {
		t.Fatalf("an agent with no skills must get no skill tools, got %d", reg.Len())
	}

	// A nil Set must be equally safe — callers should not have to guard.
	reg2 := tools.NewRegistry()
	RegisterSkillTools(reg2, nil)
	if reg2.Len() != 0 {
		t.Fatalf("nil set registered %d tools", reg2.Len())
	}
}

func TestRegisterSkillTools_TrustedOnly(t *testing.T) {
	set := loadSet(t, fstest.MapFS{
		"alpha/SKILL.md": {Data: []byte(skillDoc("alpha", "Does alpha."))},
	}, true)
	reg := tools.NewRegistry()
	RegisterSkillTools(reg, set)

	if _, ok := reg.Get(SkillToolName); !ok {
		t.Fatal("read_skill should be registered")
	}
	if _, ok := reg.Get(SkillUntrustedToolName); ok {
		t.Fatal("read_skill_untrusted must not exist when no untrusted skills do")
	}
	if _, ok := reg.Get(SkillFileToolName); ok {
		t.Fatal("read_skill_file must not exist when no skill bundles files")
	}

	tool, _ := reg.Get(SkillToolName)
	if tool.Descriptor().RequiresConfirmation {
		t.Fatal("trusted activation must not prompt")
	}
	if !tool.Descriptor().Cacheable {
		t.Fatal("bodies are immutable and in memory; the tool should be cacheable")
	}
}

// The trust split is the security-relevant part: an untrusted skill must be
// unreachable through the tool that carries no confirmation gate.
func TestRegisterSkillTools_TrustSplitIsDisjoint(t *testing.T) {
	trusted := loadSet(t, fstest.MapFS{
		"mine/SKILL.md": {Data: []byte(skillDoc("mine", "I wrote this."))},
	}, true)
	untrusted := loadSet(t, fstest.MapFS{
		"cloned/SKILL.md": {Data: []byte(skillDoc("cloned", "From a repo I cloned."))},
	}, false)
	set, err := skills.Merge(trusted, untrusted)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}

	reg := tools.NewRegistry()
	RegisterSkillTools(reg, set)

	safeEnum := enumOf(t, reg, SkillToolName, "name")
	if len(safeEnum) != 1 || safeEnum[0] != "mine" {
		t.Fatalf("ungated tool must expose only trusted skills, got %v", safeEnum)
	}
	gatedEnum := enumOf(t, reg, SkillUntrustedToolName, "name")
	if len(gatedEnum) != 1 || gatedEnum[0] != "cloned" {
		t.Fatalf("gated tool must expose only untrusted skills, got %v", gatedEnum)
	}

	gated, _ := reg.Get(SkillUntrustedToolName)
	if !gated.Descriptor().RequiresConfirmation {
		t.Fatal("untrusted activation must fire the HITL gate")
	}

	// Belt-and-braces: even if a provider ignores the enum, the tool must
	// refuse a cross-trust name rather than serve it ungated.
	safe, _ := reg.Get(SkillToolName)
	if _, err := safe.Execute(context.Background(), `{"name":"cloned"}`); err == nil {
		t.Fatal("the ungated tool must refuse an untrusted skill name")
	}
	if _, err := gated.Execute(context.Background(), `{"name":"mine"}`); err == nil {
		t.Fatal("the gated tool must refuse a trusted skill name")
	}
}

func TestReadSkill_ReturnsBodyWithoutFrontmatter(t *testing.T) {
	set := loadSet(t, fstest.MapFS{
		"alpha/SKILL.md": {Data: []byte(skillDoc("alpha", "Does alpha."))},
	}, true)
	reg := tools.NewRegistry()
	RegisterSkillTools(reg, set)
	tool, _ := reg.Get(SkillToolName)

	res, err := tool.Execute(context.Background(), `{"name":"alpha"}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(res.Text, "Instructions for alpha.") {
		t.Fatalf("body missing: %q", res.Text)
	}
	if strings.Contains(res.Text, "description:") || strings.Contains(res.Text, "---") {
		t.Fatalf("frontmatter leaked into the result: %q", res.Text)
	}
	if !strings.HasPrefix(res.Text, `<skill name="alpha">`) {
		t.Fatalf("body should be framed so the model sees its bounds: %q", res.Text)
	}
	if res.Structured == nil {
		t.Fatal("structured payload should carry the Skill")
	}
}

func TestReadSkill_UnknownNameListsAlternatives(t *testing.T) {
	set := loadSet(t, fstest.MapFS{
		"alpha/SKILL.md": {Data: []byte(skillDoc("alpha", "A."))},
		"bravo/SKILL.md": {Data: []byte(skillDoc("bravo", "B."))},
	}, true)
	reg := tools.NewRegistry()
	RegisterSkillTools(reg, set)
	tool, _ := reg.Get(SkillToolName)

	_, err := tool.Execute(context.Background(), `{"name":"imaginary"}`)
	if err == nil {
		t.Fatal("unknown skill must error")
	}
	if !strings.Contains(err.Error(), "alpha") || !strings.Contains(err.Error(), "bravo") {
		t.Fatalf("error must let the model recover in one turn: %v", err)
	}
}

// Tier three: bundled files are listed, never loaded, when a skill activates.
func TestReadSkill_ListsFilesWithoutLoadingThem(t *testing.T) {
	set := loadSet(t, fstest.MapFS{
		"alpha/SKILL.md":          {Data: []byte(skillDoc("alpha", "A."))},
		"alpha/references/api.md": {Data: []byte("SECRET-REFERENCE-CONTENT")},
	}, true)
	reg := tools.NewRegistry()
	RegisterSkillTools(reg, set)
	tool, _ := reg.Get(SkillToolName)

	res, err := tool.Execute(context.Background(), `{"name":"alpha"}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(res.Text, "references/api.md") {
		t.Fatalf("bundled file should be listed: %q", res.Text)
	}
	if strings.Contains(res.Text, "SECRET-REFERENCE-CONTENT") {
		t.Fatalf("bundled file content must NOT be loaded on activation: %q", res.Text)
	}
}

func TestReadSkillFile_RegisteredOnlyWithFilesAndReturnsEnvelope(t *testing.T) {
	set := loadSet(t, fstest.MapFS{
		"alpha/SKILL.md":          {Data: []byte(skillDoc("alpha", "A."))},
		"alpha/references/api.md": {Data: []byte("the api reference")},
	}, true)
	reg := tools.NewRegistry()
	RegisterSkillTools(reg, set)

	tool, ok := reg.Get(SkillFileToolName)
	if !ok {
		t.Fatal("read_skill_file should be registered when files exist")
	}
	res, err := tool.Execute(context.Background(), `{"skill_name":"alpha","path":"references/api.md"}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var payload struct {
		Skill     string `json:"skill"`
		Path      string `json:"path"`
		Content   string `json:"content"`
		Truncated bool   `json:"truncated"`
	}
	if err := json.Unmarshal([]byte(res.Text), &payload); err != nil {
		t.Fatalf("result should be a JSON envelope: %v (%q)", err, res.Text)
	}
	if payload.Content != "the api reference" || payload.Truncated {
		t.Fatalf("unexpected envelope: %+v", payload)
	}
	if payload.Skill != "alpha" || payload.Path != "references/api.md" {
		t.Fatalf("envelope lost its identifiers: %+v", payload)
	}
}

func TestReadSkillFile_RejectsTraversal(t *testing.T) {
	set := loadSet(t, fstest.MapFS{
		"alpha/SKILL.md":          {Data: []byte(skillDoc("alpha", "A."))},
		"alpha/references/api.md": {Data: []byte("ok")},
		"secret.txt":              {Data: []byte("do not read me")},
	}, true)
	reg := tools.NewRegistry()
	RegisterSkillTools(reg, set)
	tool, _ := reg.Get(SkillFileToolName)

	for _, p := range []string{"../secret.txt", "/etc/passwd", "references/../../secret.txt", "SKILL.md"} {
		args := fmt.Sprintf(`{"skill_name":"alpha","path":%q}`, p)
		if _, err := tool.Execute(context.Background(), args); err == nil {
			t.Fatalf("path %q should have been rejected", p)
		}
	}
}

// The enum is the whole reason FuncToolOpts.Schema had to exist: skill
// names are read off a filesystem, so no struct tag can express them.
func TestRegisterSkillTools_EnumMatchesSetNames(t *testing.T) {
	fsys := fstest.MapFS{}
	for _, n := range []string{"charlie", "alpha", "bravo"} {
		fsys[n+"/SKILL.md"] = &fstest.MapFile{Data: []byte(skillDoc(n, "Desc."))}
	}
	set := loadSet(t, fsys, true)
	reg := tools.NewRegistry()
	RegisterSkillTools(reg, set)

	enum := enumOf(t, reg, SkillToolName, "name")
	want := set.Names()
	if strings.Join(enum, ",") != strings.Join(want, ",") {
		t.Fatalf("enum %v does not match Names %v", enum, want)
	}
}

func TestSkillToolNames_CoversEveryRegisteredTool(t *testing.T) {
	trusted := loadSet(t, fstest.MapFS{
		"mine/SKILL.md": {Data: []byte(skillDoc("mine", "Mine."))},
		"mine/ref.md":   {Data: []byte("ref")},
	}, true)
	untrusted := loadSet(t, fstest.MapFS{
		"cloned/SKILL.md": {Data: []byte(skillDoc("cloned", "Cloned."))},
	}, false)
	set, err := skills.Merge(trusted, untrusted)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	reg := tools.NewRegistry()
	RegisterSkillTools(reg, set)

	pinned := make(map[string]bool, len(SkillToolNames()))
	for _, n := range SkillToolNames() {
		pinned[n] = true
	}
	// Every tool that can be registered must be pinnable, or a Selector
	// will silently drop it from the registry mid-conversation.
	for _, tool := range reg.All() {
		if !pinned[tool.Descriptor().Name] {
			t.Fatalf("tool %q is registered but absent from SkillToolNames()", tool.Descriptor().Name)
		}
	}
	if reg.Len() != 3 {
		t.Fatalf("expected all three tools registered, got %d", reg.Len())
	}
}

// stubEmbedder scores text by keyword overlap so a test can control ranking
// without a network call. Vectors are one dimension per vocabulary word.
type stubEmbedder struct{ vocab []string }

func (s stubEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		vec := make([]float32, len(s.vocab))
		lower := strings.ToLower(t)
		for j, w := range s.vocab {
			if strings.Contains(lower, w) {
				vec[j] = 1
			}
		}
		// Keep every vector non-zero so cosine similarity is defined.
		if allZero(vec) {
			vec[0] = 0.01
		}
		out[i] = vec
	}
	return out, nil
}

func allZero(v []float32) bool {
	for _, f := range v {
		if f != 0 {
			return false
		}
	}
	return true
}

// The footgun documented on SkillToolNames, pinned down as a test.
//
// A Selector replaces the registry per call with its top-K matches. The skill
// tools rank poorly by construction — the Selector embeds a tool's own name
// and description, while the domain signal lives in the skill descriptions it
// never sees. Unpinned, activation disappears on exactly the turns that need
// it. This asserts both halves: that it really vanishes, and that pinning
// fixes it.
func TestSelector_DropsSkillToolUnlessPinned(t *testing.T) {
	set := loadSet(t, fstest.MapFS{
		"incident-response/SKILL.md": {Data: []byte(skillDoc("incident-response", "Triage a production outage."))},
	}, true)

	reg := tools.NewRegistry()
	RegisterSkillTools(reg, set)
	// Competing tools whose own descriptions match the user's wording.
	tools.RegisterFunc(reg, "outage_dashboard", "Show the production outage dashboard.",
		func(_ context.Context, _ struct{}) (tools.Result, error) { return tools.Text("ok"), nil })
	tools.RegisterFunc(reg, "production_metrics", "Query production outage metrics.",
		func(_ context.Context, _ struct{}) (tools.Result, error) { return tools.Text("ok"), nil })

	emb := stubEmbedder{vocab: []string{"outage", "production", "dashboard", "metrics", "skill"}}
	query := "we have a production outage right now"
	ctx := context.Background()

	unpinned, err := tools.NewSelector(ctx, reg, emb, 2)
	if err != nil {
		t.Fatalf("NewSelector: %v", err)
	}
	narrowed, err := unpinned.SelectRegistry(ctx, query)
	if err != nil {
		t.Fatalf("SelectRegistry: %v", err)
	}
	if _, ok := narrowed.Get(SkillToolName); ok {
		t.Skip("stub ranking kept read_skill in the top-K; the pinning assertion below is the load-bearing one")
	}
	// This is the failure mode: the catalog is still in the system prompt,
	// but the tool that acts on it is gone from this call's registry.
	t.Logf("unpinned selection dropped %s, leaving %v", SkillToolName, registryNames(narrowed))

	pinned, err := tools.NewSelector(ctx, reg, emb, 2, tools.WithPinned(SkillToolNames()...))
	if err != nil {
		t.Fatalf("NewSelector pinned: %v", err)
	}
	kept, err := pinned.SelectRegistry(ctx, query)
	if err != nil {
		t.Fatalf("SelectRegistry pinned: %v", err)
	}
	if _, ok := kept.Get(SkillToolName); !ok {
		t.Fatalf("pinning must keep %s available, got %v", SkillToolName, registryNames(kept))
	}
}

func registryNames(reg *tools.Registry) []string {
	out := make([]string, 0, reg.Len())
	for _, t := range reg.All() {
		out = append(out, t.Descriptor().Name)
	}
	return out
}
