// Package fakellm is a deterministic fake LLM provider for tests.
//
// It answers every upstream protocol the proxy can forward to — OpenAI chat,
// completions, embeddings, Responses (JSON, SSE and WebSocket), Azure
// deployment paths, Anthropic messages, Gemini generate/stream/count/embed and
// OpenAI images — under any path prefix, so one instance can stand in for
// every endpoint_type. Nothing is computed: reply text and token usage are
// derived from the request bytes, so identical requests yield identical
// responses and billing assertions can be exact.
//
// Behaviour is steered, in order of precedence, by
//
//  1. a directive embedded anywhere in the request body or the X-Fake-Scenario
//     header, e.g. "[[fake status=429 retry_after=1]]" (see ParseDirective);
//  2. a per-key rule installed through POST /_control {"keys":{...}};
//  3. the global scenario installed through POST /_control.
//
// Control and inspection endpoints (/_control, /_stats, /_requests, /_reset,
// /healthz) stay compatible with the legacy Python mock provider, which this
// package supersedes.
package fakellm
