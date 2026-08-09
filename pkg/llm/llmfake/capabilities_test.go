package llmfake

import (
	"testing"

	"github.com/hung12ct/gopheragent/pkg/agent"
)

// The scripted fake is the in-tree provider that supports nothing, which is
// precisely the case a multimodal consumer needs to reject. Staying silent
// would leave it indistinguishable from a provider that simply has not
// declared itself.
func TestScriptedProviderDeclaresNoCapabilities(t *testing.T) {
	var p agent.LLMProvider = &ScriptedProvider{}

	c, ok := p.(agent.CapabilityProvider)
	if !ok {
		t.Fatalf("ScriptedProvider does not implement agent.CapabilityProvider")
	}
	if got := c.Capabilities(); got != (agent.LLMCapabilities{}) {
		t.Fatalf("Capabilities() = %+v, want the zero value", got)
	}
}
