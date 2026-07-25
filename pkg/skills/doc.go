// Package skills implements progressive disclosure of agent instructions,
// following the Agent Skills format (https://agentskills.io).
//
// # The problem it solves
//
// Reference material injected into a system prompt is paid for on every
// request, whether or not the turn needs it. An agent with twenty documented
// procedures pays twenty procedures' worth of tokens to answer "what time is
// it". Progressive disclosure splits that into three tiers:
//
//  1. Catalog — each skill's name and description, roughly 50-100 tokens
//     apiece, in the system prompt from the start. Enough for the model to
//     decide, and nothing more.
//  2. Instructions — the full SKILL.md body, delivered by a tool call only
//     when the model activates that skill.
//  3. Resources — files under the skill directory, read one at a time on
//     demand.
//
// Twenty skills cost twenty descriptions plus the one body actually used.
//
// # Why fs.FS
//
// This package loads from an io/fs.FS rather than a directory path, which is
// what makes it usable from a library rather than only from a CLI:
//
//	skills.FromFS(ctx, os.DirFS("./skills"), skills.TrustedSource())  // disk
//	skills.FromFS(ctx, embeddedFS, skills.TrustedSource())            // //go:embed
//	skills.FromFS(ctx, tenantFS)                                      // database, S3, …
//
// An embed.FS needs no filesystem at all, so skills ship inside the binary
// and work in a distroless container. A custom FS covers per-tenant content.
// And fs.ValidPath rejects ".." and absolute paths at the stdlib boundary,
// so path traversal is structurally impossible instead of guarded against.
//
// Deliberately absent: scanning $HOME, walking up to a git root, and
// merging skills by directory precedence. Those belong to CLI products that
// own the user's filesystem. In a server, $HOME belongs to a service account
// and the working directory means nothing. Adopters who want that behavior
// can build the source list themselves and pass each directory through
// os.DirFS.
//
// # Trust
//
// Trust is stated, not inferred. The caller chose which fs.FS to hand over,
// so the caller declares whether it is vouched content:
//
//	skills.FromFS(ctx, mine, skills.TrustedSource())  // content I control
//	skills.FromFS(ctx, cloned)                        // untrusted by default
//
// Untrusted is the zero value, so forgetting the option fails closed.
// Untrusted skills still load; what changes is the path the model takes to
// reach them. The activation tools build disjoint parameter enums from
// NamesByTrust and put the untrusted one behind a confirmation prompt, so a
// human sees instructions from a repository they merely cloned before the
// model does.
//
// Two things this package will not do, both permanent:
//
//   - Honor the allowed-tools frontmatter field. It is a privilege grant
//     authored by the content it would privilege. Skill.AllowedTools is
//     advisory metadata; adopters may intersect it with their registry to
//     RESTRICT a skill, never to GRANT.
//   - Execute shell commands embedded in a SKILL.md. Some implementations
//     substitute command output into the body at load time; that turns
//     cloning a repository into arbitrary code execution. Not a gap to fill.
//
// # Bounds
//
// The catalog sits in the prompt on every request and bodies stay resident
// for the process lifetime, so both are capped. See the Default* constants
// for the figures. MaxCatalogBytes is the one that matters most: without it,
// a full complement of maximum-length descriptions would render tens of
// thousands of prompt tokens per call. Admission happens at load time, so
// Catalog, Names, and Get always agree on which skills exist.
//
// Loading tolerates a malformed skill rather than failing the process — one
// bad SKILL.md must not stop an agent from starting. That trade only works
// if rejections stay visible, so log Set.Skipped once at startup.
//
// # Wiring it up
//
// With the YAML builder, a skills: block does everything. By hand:
//
//	set, err := skills.FromFS(ctx, os.DirFS("./skills"), skills.TrustedSource())
//	if err != nil {
//	    return err
//	}
//	for _, s := range set.Skipped() {
//	    log.Printf("skill skipped: %s", s)
//	}
//
//	prompt := builder.WithSkillCatalog(basePrompt, set)
//	sm := history.NewInMemSessionManager(prompt)
//
//	reg := tools.NewRegistry()
//	builtin.RegisterSkillTools(reg, set)
//	loop := agent.NewAgentLoop(sm, reg, provider)
//
// One caveat worth knowing before it bites: a tools.Selector replaces the
// registry per call with its top-K matches, and the activation tool ranks
// poorly by design — the domain signal lives in the skill descriptions, not
// in the tool's own. Pin it, or activation will vanish on exactly the turns
// that need it:
//
//	sel, _ := tools.NewSelector(ctx, reg, embedder, 8,
//	    tools.WithPinned(builtin.SkillToolNames()...))
//
// A *Set is immutable and safe to share across every session in a process.
// Load once at construction; never per request.
package skills
