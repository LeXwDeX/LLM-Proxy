package proxy

import (
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

// A single stored target with a legacy or dirty enum value must not stop the
// whole configuration from publishing: that would take the service down at
// startup.
func TestBuildTargetStatesToleratesDirtyStoredTargets(t *testing.T) {
	targets := []config.Target{
		{Name: "good", EndpointType: config.EndpointTypeOpenAI, Endpoint: "https://good.invalid", APIKey: "k"},
		{Name: "auth-case", EndpointType: config.EndpointTypeClaude, Endpoint: "https://auth.invalid", APIKey: "k", AuthMode: " Bearer "},
		{Name: "auth-unknown", EndpointType: config.EndpointTypeClaude, Endpoint: "https://auth2.invalid", APIKey: "k", AuthMode: "x-api-key"},
		{Name: "image-typo", EndpointType: config.EndpointTypeOpenAIImage, Endpoint: "https://img.invalid", APIKey: "k", ImageOperation: "edit"},
		{Name: "bad-type", EndpointType: "no-such-provider", Endpoint: "https://bad.invalid", APIKey: "k"},
		{Name: "bad-adapter", EndpointType: config.EndpointTypeOpenAI, Endpoint: "https://adapter.invalid", APIKey: "k", UsageAdapter: "no-such-adapter"},
	}
	parsed, order, err := buildTargetStates(targets, newTestLogger())
	if err != nil {
		t.Fatalf("one dirty target failed the whole configuration: %v", err)
	}
	if parsed["good"] == nil {
		t.Fatal("valid target was not published")
	}
	// Stored values keep their pre-validation meaning: only exact "bearer"
	// selects bearer auth, so " Bearer " still uses the provider default.
	if got := parsed["auth-case"]; got == nil || got.Target().AuthMode != "" {
		t.Fatalf("stored case variant changed auth semantics: %+v", got)
	}
	// Unknown auth modes keep the provider default, as before validation existed.
	if got := parsed["auth-unknown"]; got == nil || got.Target().AuthMode != "" {
		t.Fatalf("unknown auth mode did not fall back to the provider default: %+v", got)
	}
	// An unknown image operation keeps the target published but unroutable.
	if got := parsed["image-typo"]; got == nil || got.Target().SupportsPath("/v1/images/edits") {
		t.Fatalf("unknown image operation became routable or dropped: %+v", got)
	}
	for _, name := range []string{"bad-type", "bad-adapter"} {
		if parsed[name] != nil {
			t.Fatalf("target %s with an unsupported provider setting was published", name)
		}
	}
	if len(order) != len(parsed) {
		t.Fatalf("order and index disagree: %d vs %d", len(order), len(parsed))
	}
}
