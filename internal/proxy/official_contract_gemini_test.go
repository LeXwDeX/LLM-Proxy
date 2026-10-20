package proxy

import "testing"

// The gemini-openai-compat-* cases use the Gemini OpenAI compatibility layer
// (https://ai.google.dev/gemini-api/docs/openai). Their usage shape is a
// redacted real response observed on 2026-10-20 (reasoning and text reported as
// disjoint output parts), not an official documentation example.
func TestOfficialContractGemini(t *testing.T) { runOfficialContracts(t, "gemini") }
