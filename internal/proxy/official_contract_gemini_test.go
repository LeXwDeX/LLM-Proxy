package proxy

import "testing"

// The gemini-openai-compat-* usage comes from the redacted Chat JSON observation
// in https://github.com/LeXwDeX/LLM-Proxy/issues/6 (2026-10-20), not an official
// example. Requests and response envelopes are synthetic, with a neutral model
// name. The SSE case is a synthetic extension; no real SSE was observed.
// The fixture schema has no note field, so provenance is recorded here.
func TestOfficialContractGemini(t *testing.T) { runOfficialContracts(t, "gemini") }
