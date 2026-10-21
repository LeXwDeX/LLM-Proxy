package proxy

import "testing"

// The gemini-openai-compat-* cases use the Gemini OpenAI compatibility layer
// (https://ai.google.dev/gemini-api/docs/openai). The official page has no usage
// example; their usage shape comes from the redacted real sample in issue #6
// (observed 2026-10-20, reasoning and text reported as disjoint output parts).
// The fixture schema has no note field, so provenance is recorded here.
func TestOfficialContractGemini(t *testing.T) { runOfficialContracts(t, "gemini") }
