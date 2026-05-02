package history

import "strings"

// promptVersionPrefix is the marker prepended to a session's stored system
// message when SessionManager.PromptVersion is set. The marker is an HTML
// comment so any LLM that surfaces the system prompt verbatim renders it
// invisibly. Operators querying the storage layer (e.g. SELECT messages
// FROM agent_sessions) can grep for this marker to audit which prompt
// version each session is running.
const promptVersionPrefix = "<!-- prompt-version:"
const promptVersionSuffix = " -->\n"

// stampPromptVersion prepends a prompt-version marker to body. Empty
// version returns body unchanged so this is zero-overhead when the
// feature is unused.
func stampPromptVersion(version, body string) string {
	if version == "" {
		return body
	}
	return promptVersionPrefix + version + promptVersionSuffix + body
}

// extractPromptVersion peels off any prompt-version marker at the start
// of content and returns (version, remaining). If the content does not
// start with a marker, version is "" and remaining is content unchanged.
// Used by tests / migration tooling that wants to inspect the stored
// version without parsing the full system message body.
func extractPromptVersion(content string) (version, remaining string) {
	rest, ok := strings.CutPrefix(content, promptVersionPrefix)
	if !ok {
		return "", content
	}
	idx := strings.Index(rest, promptVersionSuffix)
	if idx < 0 {
		return "", content
	}
	return rest[:idx], rest[idx+len(promptVersionSuffix):]
}
