package proxy

import (
	"context"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/nosql"
	"github.com/LeXwDeX/LLM-Proxy/internal/pricing"
	"github.com/LeXwDeX/LLM-Proxy/internal/requeststats"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

// Prices are synthetic and only make the expected cost exact.
var finalSnapshotRates = usage.CostTable{
	"test-model": {InputPer1MTokens: 2, OutputPer1MTokens: 4, CacheReadPer1MToken: 5},
	"advisor":    {InputPer1MTokens: 10, OutputPer1MTokens: 20},
	"fallback":   {InputPer1MTokens: 30, OutputPer1MTokens: 40},
}

const (
	snapshotStart = "data: {\"type\":\"message_start\",\"message\":{\"model\":\"test-model\",\"usage\":{\"input_tokens\":7,\"output_tokens\":0}}}\n\n"
	snapshotStop  = "data: {\"type\":\"message_stop\"}\n\n"
	snapshotError = "data: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\"}}\n\n"
	snapshotEmpty = "data: {\"type\":\"message_delta\",\"delta\":{},\"usage\":{}}\n\n"
	snapshotTail  = "data: {\"type\":\"message_delta\",\"delta\":{},\"usage\":{\"output_tokens\":2}}\n\n"
	snapshotFinal = `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":7,"output_tokens":2,"cache_read_input_tokens":0,"cache_creation_input_tokens":0,"iterations":[{"type":"message","input_tokens":7,"output_tokens":2,"cache_read_input_tokens":0,"cache_creation_input_tokens":0},{"type":"advisor_message","model":"advisor","input_tokens":3,"output_tokens":1,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}]}}` + "\n\n"
)

// ledgerService is the real Service with a temporary bbolt usage store, so the
// test reads the persisted bundle and its cost rather than the parser alone.
func ledgerService(t *testing.T, upstream string) (*Service, *nosql.UsageStore) {
	t.Helper()
	return ledgerServiceFor(t, upstream, config.EndpointTypeClaude)
}

func ledgerServiceFor(t *testing.T, upstream, endpointType string) (*Service, *nosql.UsageStore) {
	t.Helper()
	s, _, _, _ := goneService(t, upstream, endpointType, config.UsageAdapterStandard)
	db, err := nosql.OpenDB(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store := nosql.NewUsageStore(db)
	s.SetUsageRecorder(store)
	return s, store
}

// snapshotLedger returns the persisted rows and their exact resolved cost.
func snapshotLedger(t *testing.T, store *nosql.UsageStore) ([]usage.Event, float64) {
	t.Helper()
	return snapshotLedgerWith(t, store, finalSnapshotRates)
}

func snapshotLedgerWith(t *testing.T, store *nosql.UsageStore, resolver usage.RateResolver) ([]usage.Event, float64) {
	t.Helper()
	rows, err := store.List(usage.Filter{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) == 0 {
		return rows, 0
	}
	from := rows[0].Timestamp.Truncate(time.Hour)
	to := from.Add(time.Hour)
	units, err := store.ReadCostUnits(context.Background(), usage.Filter{From: &from, To: &to}, resolver, true)
	if err != nil {
		t.Fatal(err)
	}
	var cost float64
	for _, unit := range units {
		cost += unit.CostUSD
	}
	return rows, cost
}

// tierRateResolver prices the standard tier from base rates and the fast tier
// from a condition. Only a polluted pricing context can select the fast rate.
func tierRateResolver() *pricing.Resolver {
	return pricing.NewResolver([]pricing.PricingTemplate{{
		ID:        "final-snapshot-tier",
		Models:    []string{"test-model"},
		Mode:      pricing.ModeFlat,
		BaseRates: usage.CostRates{InputPer1MTokens: 2, OutputPer1MTokens: 4},
		Conditions: []pricing.PriceCondition{{
			ID:          "fast",
			ServiceTier: "fast",
			Rates:       usage.CostRates{InputPer1MTokens: 20, OutputPer1MTokens: 40},
		}},
	}}, nil)
}

func serveSnapshot(t *testing.T, s *Service, wire, path, body string) {
	t.Helper()
	w := httptest.NewRecorder()
	s.ServeHTTP(w, goneRequest(t, path, body))
	if w.Code != http.StatusOK || w.Body.String() != wire {
		t.Fatalf("transport changed: code=%d body=%q", w.Code, w.Body.String())
	}
}

func rootAndChildren(t *testing.T, rows []usage.Event) (*usage.Event, []usage.Event) {
	t.Helper()
	var root *usage.Event
	var children []usage.Event
	for i := range rows {
		if rows[i].Supplementary {
			children = append(children, rows[i])
			continue
		}
		if root != nil {
			t.Fatalf("multiple root rows: %+v", rows)
		}
		root = &rows[i]
	}
	if root == nil {
		t.Fatalf("no root row: %+v", rows)
	}
	return root, children
}

// An untrusted tail after confirmed usage must not erase the ledger: the
// probe's exact sequence keeps root 7/2 plus advisor 3/1 and $0.000072.
func TestClaudeConfirmedFinalSnapshotSurvivesUntrustedTail(t *testing.T) {
	for _, tc := range []struct {
		name, tail string
		failed     bool
		diagnostic string
	}{
		{"nonfinal_output_eof", snapshotTail, false, diagnosticInterruptedAfterFinalUsage},
		{"empty_delta_stop", snapshotEmpty + snapshotStop, false, ""},
		{"error_then_empty_stop", snapshotError + snapshotEmpty + snapshotStop, true, diagnosticInterruptedAfterFinalUsage},
		{"final_stop_control", snapshotStop, false, ""},
		{"final_error_control", snapshotError, true, diagnosticInterruptedAfterFinalUsage},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wire := snapshotStart + snapshotFinal + tc.tail
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, wire)
			}))
			defer upstream.Close()
			s, store := ledgerService(t, upstream.URL)
			serveSnapshot(t, s, wire, "/v1/messages", `{"model":"test-model","stream":true,"max_tokens":100,"messages":[]}`)
			rows, cost := snapshotLedger(t, store)
			if len(rows) != 2 || math.Abs(cost-.000072) > 1e-12 {
				t.Fatalf("records=%d cost=%.9f want 2/.000072: %+v", len(rows), cost, rows)
			}
			root, children := rootAndChildren(t, rows)
			if root.Model != "test-model" || root.InputTokens != 7 || root.OutputTokens != 2 || root.UsageStatus != usageStatusComplete || root.CostIncomplete || root.NonBillable {
				t.Fatalf("root=%+v", root)
			}
			if len(children) != 1 || children[0].Model != "advisor" || children[0].InputTokens != 3 || children[0].OutputTokens != 1 || children[0].CostIncomplete {
				t.Fatalf("children=%+v", children)
			}
			if tc.diagnostic == "" {
				if hasGoneDiagnostic(root.UsageDiagnostic, diagnosticInterruptedAfterFinalUsage) || hasGoneDiagnostic(root.UsageDiagnostic, diagnosticUpstreamInterruptedNoUsage) {
					t.Fatalf("clean final diagnostic=%q", root.UsageDiagnostic)
				}
			} else if !hasGoneDiagnostic(root.UsageDiagnostic, tc.diagnostic) || hasGoneDiagnostic(root.UsageDiagnostic, diagnosticUpstreamInterruptedNoUsage) {
				t.Fatalf("diagnostic=%q want %s", root.UsageDiagnostic, tc.diagnostic)
			}
			if tc.failed && root.ResponseStatus != "failed" {
				t.Fatalf("lifecycle failure lost: %+v", root)
			}
			if !tc.failed && root.ResponseStatus == "failed" {
				t.Fatalf("unexpected failure: %+v", root)
			}
		})
	}
}

// A genuinely newer final snapshot replaces the committed one, including an
// explicit empty iterations array that removes the earlier advisor component.
func TestClaudeNewFinalSnapshotReplacesConfirmed(t *testing.T) {
	replacement := func(iterations, in, out string) string {
		return `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":` + in + `,"output_tokens":` + out + `,"cache_read_input_tokens":0,"cache_creation_input_tokens":0,"iterations":` + iterations + `}}` + "\n\n"
	}
	for _, tc := range []struct {
		name, second string
		rows         int
		rootIn       int64
		rootOut      int64
		childIn      int64
		childOut     int64
		cost         float64
	}{
		{
			name:   "new_counts_and_advisor",
			second: replacement(`[{"type":"message","input_tokens":11,"output_tokens":4,"cache_read_input_tokens":0,"cache_creation_input_tokens":0},{"type":"advisor_message","model":"advisor","input_tokens":5,"output_tokens":2,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}]`, "11", "4"),
			rows:   2, rootIn: 11, rootOut: 4, childIn: 5, childOut: 2, cost: .000128,
		},
		{
			name:   "explicit_empty_iterations",
			second: replacement(`[]`, "11", "4"),
			rows:   1, rootIn: 11, rootOut: 4, cost: .000038,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wire := snapshotStart + snapshotFinal + tc.second + snapshotStop
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, wire)
			}))
			defer upstream.Close()
			s, store := ledgerService(t, upstream.URL)
			serveSnapshot(t, s, wire, "/v1/messages", `{"model":"test-model","stream":true,"max_tokens":100,"messages":[]}`)
			rows, cost := snapshotLedger(t, store)
			if len(rows) != tc.rows || math.Abs(cost-tc.cost) > 1e-12 {
				t.Fatalf("records=%d cost=%.9f want %d/%.9f: %+v", len(rows), cost, tc.rows, tc.cost, rows)
			}
			root, children := rootAndChildren(t, rows)
			if root.InputTokens != tc.rootIn || root.OutputTokens != tc.rootOut || root.UsageStatus != usageStatusComplete || root.CostIncomplete {
				t.Fatalf("replacement not committed: %+v", root)
			}
			if tc.rows == 1 {
				if len(children) != 0 || len(root.Components) != 0 || hasGoneDiagnostic(root.UsageDiagnostic, "missing_final_iterations") {
					t.Fatalf("explicit empty iterations kept old evidence: %+v", root)
				}
				return
			}
			if len(children) != 1 || children[0].InputTokens != tc.childIn || children[0].OutputTokens != tc.childOut || children[0].Model != "advisor" {
				t.Fatalf("replacement child=%+v", children)
			}
		})
	}
}

// A conflicting or invalid later final must not overwrite the committed one.
func TestClaudeInvalidLaterFinalDoesNotOverwriteConfirmed(t *testing.T) {
	for _, tc := range []struct {
		name, second string
	}{
		{"null_output", `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":11,"output_tokens":null}}` + "\n\n"},
		{"invalid_output", `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":11,"output_tokens":"4"}}` + "\n\n"},
		{"conflicting_thinking", `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":11,"output_tokens":2,"output_tokens_details":{"thinking_tokens":3}}}` + "\n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wire := snapshotStart + snapshotFinal + tc.second + snapshotStop
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, wire)
			}))
			defer upstream.Close()
			s, store := ledgerService(t, upstream.URL)
			serveSnapshot(t, s, wire, "/v1/messages", `{"model":"test-model","stream":true,"max_tokens":100,"messages":[]}`)
			rows, cost := snapshotLedger(t, store)
			if len(rows) != 2 || math.Abs(cost-.000072) > 1e-12 {
				t.Fatalf("confirmed evidence replaced: records=%d cost=%.9f %+v", len(rows), cost, rows)
			}
			root, children := rootAndChildren(t, rows)
			if root.InputTokens != 7 || root.OutputTokens != 2 || root.UsageStatus != usageStatusComplete || root.CostIncomplete || len(children) != 1 {
				t.Fatalf("confirmed evidence changed: root=%+v children=%+v", root, children)
			}
		})
	}
}

// Free root, paid child and fallback attribution survive the same tail.
func TestClaudeFinalSnapshotAttributionSurvivesTail(t *testing.T) {
	t.Run("free_root_paid_advisor", func(t *testing.T) {
		final := `data: {"type":"message_delta","delta":{"stop_reason":"refusal","stop_details":{"category":"cyber"}},"usage":{"input_tokens":7,"output_tokens":0,"cache_read_input_tokens":0,"cache_creation_input_tokens":0,"iterations":[{"type":"message","input_tokens":7,"output_tokens":0,"cache_read_input_tokens":0,"cache_creation_input_tokens":0},{"type":"advisor_message","model":"advisor","input_tokens":3,"output_tokens":1,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}]}}` + "\n\n"
		wire := snapshotStart + final + `data: {"type":"message_delta","delta":{},"usage":{"output_tokens":0}}` + "\n\n"
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, wire)
		}))
		defer upstream.Close()
		s, store := ledgerService(t, upstream.URL)
		serveSnapshot(t, s, wire, "/v1/messages", `{"model":"test-model","stream":true,"max_tokens":100,"messages":[]}`)
		rows, cost := snapshotLedger(t, store)
		if len(rows) != 2 || math.Abs(cost-.00005) > 1e-12 {
			t.Fatalf("records=%d cost=%.9f want 2/.00005: %+v", len(rows), cost, rows)
		}
		root, children := rootAndChildren(t, rows)
		if !root.NonBillable || root.OutputTokens != 0 || len(children) != 1 || children[0].NonBillable || children[0].Model != "advisor" {
			t.Fatalf("root=%+v children=%+v", root, children)
		}
	})
	t.Run("fallback_attribution", func(t *testing.T) {
		final := `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":7,"output_tokens":2,"cache_read_input_tokens":0,"cache_creation_input_tokens":0,"iterations":[{"type":"message","input_tokens":13,"output_tokens":4,"cache_read_input_tokens":0,"cache_creation_input_tokens":0},{"type":"fallback_message","model":"fallback","input_tokens":7,"output_tokens":2,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}]}}` + "\n\n"
		wire := snapshotStart + final + snapshotTail
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, wire)
		}))
		defer upstream.Close()
		s, store := ledgerService(t, upstream.URL)
		serveSnapshot(t, s, wire, "/v1/messages", `{"model":"test-model","stream":true,"max_tokens":100,"messages":[]}`)
		rows, cost := snapshotLedger(t, store)
		if len(rows) != 2 || math.Abs(cost-.000332) > 1e-12 {
			t.Fatalf("records=%d cost=%.9f want 2/.000332: %+v", len(rows), cost, rows)
		}
		root, children := rootAndChildren(t, rows)
		if root.Model != "fallback" || root.InputTokens != 7 || root.OutputTokens != 2 || len(children) != 1 || children[0].Model != "test-model" || children[0].InputTokens != 13 || children[0].OutputTokens != 4 {
			t.Fatalf("root=%+v children=%+v", root, children)
		}
	})
}

// A client disconnect after confirmed usage keeps the official values and the
// provider side is still canceled at once, without a second call.
func TestClaudeClientGoneAfterFinalTailRecordsOfficial(t *testing.T) {
	var calls atomic.Int32
	upstreamCanceled := make(chan struct{})
	wire := snapshotStart + snapshotFinal + snapshotTail + ": final-sent\n\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, wire)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(upstreamCanceled)
	}))
	defer upstream.Close()
	s, events, stats, _ := goneService(t, upstream.URL, config.EndpointTypeClaude, config.UsageAdapterStandard)
	w := newGoneClientWriter()
	cancel, done := serveGone(s, w, goneRequest(t, "/v1/messages", `{"model":"test-model","stream":true,"max_tokens":100,"messages":[]}`))
	defer cancel()
	waitGoneBody(t, w, "final-sent")
	cancel()
	awaitGone(t, upstreamCanceled, "upstream cancellation")
	awaitGone(t, done, "request end")
	got := events.snapshot()
	if len(got) != 1 {
		t.Fatalf("usage events=%d want 1: %+v", len(got), got)
	}
	ev := got[0]
	if ev.InputTokens != 7 || ev.OutputTokens != 2 || ev.UsageStatus != usageStatusComplete || ev.CostIncomplete || len(ev.Components) != 1 || ev.Components[0].Model != "advisor" {
		t.Fatalf("confirmed usage lost on disconnect: %+v", ev)
	}
	if !hasGoneDiagnostic(ev.UsageDiagnostic, diagnosticClientDisconnectedAfterFinalUsage) || hasGoneDiagnostic(ev.UsageDiagnostic, diagnosticClientDisconnectedNoUsage) {
		t.Fatalf("diagnostic=%q", ev.UsageDiagnostic)
	}
	if s.MetricsSnapshot().TotalUsageClientGoneNoUsage != 0 {
		t.Error("official usage counted as no-usage")
	}
	assertCompletions(t, stats, requeststats.Canceled)
	assertNoReplay(t, s, calls.Load())
	assertRejectionResources(t, s)
}

func TestConfirmedSnapshotKeepsModelComponentsAndPricing(t *testing.T) {
	const finalWithTier = `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":7,"output_tokens":2,"cache_read_input_tokens":0,"cache_creation_input_tokens":0,"service_tier":"standard","iterations":[{"type":"message","input_tokens":7,"output_tokens":2},{"type":"advisor_message","model":"advisor","input_tokens":3,"output_tokens":1}]}}` + "\n\n"
	p := usageParser{protocol: config.ProtocolAnthropicMessages, model: "executor", requestBody: []byte(`{"service_tier":"standard"}`)}
	wire := snapshotStart + finalWithTier +
		`data: {"type":"message_delta","delta":{},"usage":{"output_tokens":9,"service_tier":"priority"},"model":"mutated"}` + "\n\n"
	u := newUsageStream("text/event-stream", "", p)
	_, _ = io.WriteString(u, wire)
	u.Close()
	if !u.finalSeen || u.finalTokens.OutputTokens != 2 || u.finalTokens.InputTokens != 7 || u.finalTokens.status != usageStatusComplete || len(u.finalTokens.components) != 1 || u.finalTokens.components[0].Model != "advisor" {
		t.Fatalf("tail recomputed the committed tokens: %+v", u.finalTokens)
	}
	if u.finalModel != "test-model" || u.finalTokens.pricingContext == nil || u.finalTokens.pricingContext.ResponseServiceTier.Raw != "standard" {
		t.Fatalf("model or pricing context changed: model=%q pricing=%+v", u.finalModel, u.finalTokens.pricingContext)
	}
}

// Responses also commits only on its terminal event; a later non-terminal
// response envelope may not rewrite it, while a newer terminal may.
func TestResponsesFinalSnapshotTailAndReplacement(t *testing.T) {
	p := usageParser{protocol: config.ProtocolOpenAIResponses}
	completed := func(in, out string) string {
		return `data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":` + in + `,"output_tokens":` + out + `}}}` + "\n\n"
	}
	t.Run("nonterminal_tail", func(t *testing.T) {
		wire := completed("11", "7") + `data: {"type":"response.in_progress","response":{"status":"in_progress","usage":{"input_tokens":100,"output_tokens":0}}}` + "\n\n"
		u := newUsageStream("text/event-stream", "", p)
		_, _ = io.WriteString(u, wire)
		u.Close()
		if !u.finalSeen || u.tokens.InputTokens != 11 || u.tokens.OutputTokens != 7 || u.tokens.status != usageStatusComplete {
			t.Fatalf("non-terminal tail rewrote the snapshot: %+v", u.tokens)
		}
	})
	t.Run("new_terminal_replaces", func(t *testing.T) {
		wire := completed("11", "7") + completed("20", "5")
		u := newUsageStream("text/event-stream", "", p)
		_, _ = io.WriteString(u, wire)
		u.Close()
		if !u.finalSeen || u.tokens.InputTokens != 20 || u.tokens.OutputTokens != 5 || u.tokens.status != usageStatusComplete {
			t.Fatalf("new terminal did not replace: %+v", u.tokens)
		}
	})
}

// An unknown iterations shape in a non-final tail is not final evidence and
// must not replace the committed components.
func TestConfirmedSnapshotIgnoresUnknownIterationsTail(t *testing.T) {
	p := usageParser{protocol: config.ProtocolAnthropicMessages, model: "executor"}
	wire := snapshotStart + snapshotFinal +
		`data: {"type":"message_delta","delta":{},"usage":{"output_tokens":2,"iterations":[{"type":"unknown"}]}}` + "\n\n"
	u := newUsageStream("text/event-stream", "", p)
	_, _ = io.WriteString(u, wire)
	u.Close()
	if !u.finalSeen || u.tokens.InputTokens != 7 || u.tokens.OutputTokens != 2 || u.tokens.costIncomplete || len(u.tokens.components) != 1 || u.tokens.components[0].Model != "advisor" {
		t.Fatalf("unknown tail replaced the snapshot: %+v", u.tokens)
	}
}

// A Chat usage chunk commits the official final usage. A later non-usage tail
// with a different model and service tier is lifecycle-only: it must not
// rewrite the response model or pricing context, and [DONE] must not re-commit
// the polluted working copy.
func TestChatFinalSnapshotTailDoesNotReprice(t *testing.T) {
	p := usageParser{protocol: config.ProtocolOpenAIChat, model: "test-model", requestBody: []byte(`{"service_tier":"default"}`)}
	wire := `data: {"model":"test-model","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":2},"service_tier":"default"}` + "\n\n" +
		`data: {"model":"mutated","choices":[{"index":0,"delta":{}}],"service_tier":"priority"}` + "\n\n" +
		"data: [DONE]\n\n"
	u := newUsageStream("text/event-stream", "", p)
	_, _ = io.WriteString(u, wire)
	u.Close()
	if !u.finalSeen || u.tokens.InputTokens != 7 || u.tokens.OutputTokens != 2 || u.tokens.status != usageStatusComplete || u.tokens.costIncomplete {
		t.Fatalf("final usage changed: %+v", u.tokens)
	}
	if u.finalModel != "test-model" || u.model != "test-model" {
		t.Fatalf("tail model leaked: final=%q model=%q", u.finalModel, u.model)
	}
	if c := u.tokens.pricingContext; c == nil || c.ResponseServiceTier.Raw != "default" || c.ActualServiceTier != "standard" {
		t.Fatalf("tail priced the final usage: %+v", c)
	}
}

// The same Chat sequence must bill the standard tier and keep the response
// model after the real Service persisted the bundle.
func TestChatFinalSnapshotTailBilling(t *testing.T) {
	wire := `data: {"model":"test-model","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":2},"service_tier":"default"}` + "\n\n" +
		`data: {"model":"mutated","choices":[{"index":0,"delta":{}}],"service_tier":"priority"}` + "\n\n" +
		"data: [DONE]\n\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, wire)
	}))
	defer upstream.Close()
	s, store := ledgerServiceFor(t, upstream.URL, config.EndpointTypeOpenAI)
	serveSnapshot(t, s, wire, "/v1/chat/completions", `{"model":"test-model","stream":true,"messages":[],"stream_options":{"include_usage":true}}`)
	rows, cost := snapshotLedgerWith(t, store, tierRateResolver())
	if len(rows) != 1 || math.Abs(cost-.000022) > 1e-12 {
		t.Fatalf("records=%d cost=%.9f want 1/.000022: %+v", len(rows), cost, rows)
	}
	root, _ := rootAndChildren(t, rows)
	if root.Model != "test-model" || root.ResponseModel != "test-model" || root.InputTokens != 7 || root.OutputTokens != 2 || root.UsageStatus != usageStatusComplete || root.CostIncomplete {
		t.Fatalf("root=%+v", root)
	}
	if c := root.PricingContext; c == nil || c.ResponseServiceTier.Raw != "default" || c.ActualServiceTier != "standard" {
		t.Fatalf("pricing context=%+v", c)
	}
}

// An invalid Anthropic final candidate is validated before it may replace any
// state. Its cache/count model evidence must not feed the next valid final.
func TestClaudeRejectedFinalCandidateLeavesNoResidue(t *testing.T) {
	const invalid = `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":11,"output_tokens":null,"cache_read_input_tokens":1000}}` + "\n\n"
	const recovery = `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":7,"output_tokens":4,"iterations":[]}}` + "\n\n"
	wire := snapshotStart + snapshotFinal + invalid + recovery + snapshotStop
	u := newUsageStream("text/event-stream", "", usageParser{protocol: config.ProtocolAnthropicMessages, model: "test-model"})
	_, _ = io.WriteString(u, wire)
	u.Close()
	if !u.finalSeen || u.finalModel != "test-model" {
		t.Fatalf("no committed final: seen=%v model=%q", u.finalSeen, u.finalModel)
	}
	for name, tokens := range map[string]usageTokens{"working": u.tokens, "committed": u.finalTokens} {
		if tokens.InputTokens != 7 || tokens.OutputTokens != 4 || tokens.CachedTokens != 0 || tokens.status != usageStatusComplete || tokens.costIncomplete || len(tokens.components) != 0 {
			t.Fatalf("%s tokens kept rejected evidence: %+v", name, tokens)
		}
	}
}

// The persisted bundle and its cost show the same rule: 7/4 with no cache and
// no advisor component.
func TestClaudeRejectedFinalCandidateBilling(t *testing.T) {
	const invalid = `data: {"type":"message_delta","model":"mutated","service_tier":"priority","speed":"fast","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":11,"output_tokens":null,"cache_read_input_tokens":1000,"service_tier":"priority","speed":"fast"}}` + "\n\n"
	const recovery = `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":7,"output_tokens":4,"iterations":[]}}` + "\n\n"
	wire := snapshotStart + snapshotFinal + invalid + recovery + snapshotStop
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, wire)
	}))
	defer upstream.Close()
	s, store := ledgerService(t, upstream.URL)
	serveSnapshot(t, s, wire, "/v1/messages", `{"model":"test-model","stream":true,"max_tokens":100,"messages":[]}`)
	rows, cost := snapshotLedger(t, store)
	if len(rows) != 1 || math.Abs(cost-.00003) > 1e-12 {
		t.Fatalf("records=%d cost=%.9f want 1/.00003: %+v", len(rows), cost, rows)
	}
	root, children := rootAndChildren(t, rows)
	if len(children) != 0 || root.Model != "test-model" || root.ResponseModel != "test-model" || root.InputTokens != 7 || root.OutputTokens != 4 || root.CachedTokens != 0 || root.UsageStatus != usageStatusComplete || root.CostIncomplete {
		t.Fatalf("root=%+v children=%+v", root, children)
	}
	c := root.PricingContext
	if c == nil || c.ResponseServiceTier.State != usage.PricingValueMissing || c.ActualServiceTier != "" || c.ResponseSpeed.State != usage.PricingValueMissing || c.ActualSpeed != "" {
		t.Fatalf("rejected pricing leaked: %+v", c)
	}
}

func TestOpenAIRejectedFinalSnapshotKeepsModelTierAndBilling(t *testing.T) {
	for _, tc := range []struct{ name, path, wire string }{
		{"chat", "/v1/chat/completions",
			`data: {"model":"test-model","service_tier":"default","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":2}}` + "\n\n" +
				`data: {"model":"mutated","service_tier":"priority","choices":[{"index":0,"delta":{}}],"usage":{"prompt_tokens":11,"completion_tokens":null}}` + "\n\n" +
				`data: {"choices":[{"index":0,"delta":{}}],"usage":{"prompt_tokens":7,"completion_tokens":4}}` + "\n\ndata: [DONE]\n\n"},
		{"responses", "/v1/responses",
			`data: {"type":"response.completed","response":{"model":"test-model","status":"completed","service_tier":"default","usage":{"input_tokens":7,"output_tokens":2}}}` + "\n\n" +
				`data: {"type":"response.completed","response":{"model":"mutated","status":"completed","service_tier":"priority","usage":{"input_tokens":11,"output_tokens":null}}}` + "\n\n" +
				`data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":7,"output_tokens":4}}}` + "\n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, tc.wire)
			}))
			defer upstream.Close()
			s, store := ledgerServiceFor(t, upstream.URL, config.EndpointTypeOpenAI)
			serveSnapshot(t, s, tc.wire, tc.path, `{"model":"test-model","stream":true,"messages":[],"stream_options":{"include_usage":true}}`)
			rows, cost := snapshotLedgerWith(t, store, tierRateResolver())
			if len(rows) != 1 || math.Abs(cost-.00003) > 1e-12 {
				t.Fatalf("records=%d cost=%.9f want 1/.00003: %+v", len(rows), cost, rows)
			}
			root, _ := rootAndChildren(t, rows)
			if root.ResponseModel != "test-model" || root.InputTokens != 7 || root.OutputTokens != 4 || root.UsageStatus != usageStatusComplete || root.CostIncomplete {
				t.Fatalf("root=%+v", root)
			}
			if c := root.PricingContext; c == nil || c.ResponseServiceTier.Raw != "default" || c.ActualServiceTier != "standard" {
				t.Fatalf("pricing context=%+v", c)
			}
		})
	}
}

func TestClaudeFirstConflictingFinalKeepsConflictEvidence(t *testing.T) {
	wire := snapshotStart + `data: {"type":"message_delta","delta":{},"usage":{"output_tokens":1}}` + "\n\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":11,"output_tokens":2,"output_tokens_details":{"thinking_tokens":3}}}` + "\n\n" + snapshotStop
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, wire)
	}))
	defer upstream.Close()
	s, store := ledgerService(t, upstream.URL)
	serveSnapshot(t, s, wire, "/v1/messages", `{"model":"test-model","stream":true,"max_tokens":100,"messages":[]}`)
	rows, cost := snapshotLedger(t, store)
	if len(rows) != 1 || math.Abs(cost-.00003) > 1e-12 {
		t.Fatalf("records=%d cost=%.9f want 1/.00003: %+v", len(rows), cost, rows)
	}
	root, _ := rootAndChildren(t, rows)
	if root.UsageStatus != usage.UsageStatusConflict || root.InputTokens != 11 || root.OutputTokens != 2 || root.ReasoningTokens != 3 ||
		!root.CostIncomplete || !hasGoneDiagnostic(root.UsageDiagnostic, "reasoning_exceeds_output") {
		t.Fatalf("conflict evidence lost: %+v", root)
	}
}

func TestRejectedFinalSnapshotClonesDeepUsageDetails(t *testing.T) {
	imageRate, cachedImageRate := 10.0, 5.0
	rates := usage.CostTable{"test-model": {InputPer1MTokens: 2, OutputPer1MTokens: 4, CacheReadPer1MToken: 1, ImageInputPer1MTokens: &imageRate, CachedImageInputPer1MTokens: &cachedImageRate}}
	for _, tc := range []struct{ name, path, wire string }{
		{"chat", "/v1/chat/completions",
			`data: {"model":"test-model","choices":[{"index":0,"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":4,"image_tokens":6,"cached_tokens_details":{"image_tokens":2}}}}` + "\n\n" +
				`data: {"choices":[],"usage":{"completion_tokens":null,"prompt_tokens_details":{"cached_tokens_details":{"image_tokens":3}}}}` + "\n\n" +
				`data: {"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":4}}` + "\n\ndata: [DONE]\n\n"},
		{"responses", "/v1/responses",
			`data: {"type":"response.completed","response":{"model":"test-model","status":"completed","usage":{"input_tokens":10,"output_tokens":2,"input_tokens_details":{"cached_tokens":4,"image_tokens":6,"cached_tokens_details":{"image_tokens":2}}}}}` + "\n\n" +
				`data: {"type":"response.completed","response":{"status":"completed","usage":{"output_tokens":null,"input_tokens_details":{"cached_tokens_details":{"image_tokens":3}}}}}` + "\n\n" +
				`data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":10,"output_tokens":4}}}` + "\n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, tc.wire)
			}))
			defer upstream.Close()
			s, store := ledgerServiceFor(t, upstream.URL, config.EndpointTypeOpenAI)
			serveSnapshot(t, s, tc.wire, tc.path, `{"model":"test-model","stream":true,"messages":[]}`)
			rows, cost := snapshotLedgerWith(t, store, rates)
			if len(rows) != 1 || math.Abs(cost-.000072) > 1e-12 {
				t.Fatalf("records=%d cost=%.9f want 1/.000072: %+v", len(rows), cost, rows)
			}
			root, _ := rootAndChildren(t, rows)
			if root.InputTokens != 6 || root.CachedTokens != 4 || root.OutputTokens != 4 || root.ImageInputTokens != 4 || root.CachedImageInputTokens != 2 {
				t.Fatalf("rejected partition leaked: %+v", root)
			}
		})
	}
}

func TestGeminiFinalSnapshotTailKeepsModel(t *testing.T) {
	p := usageParser{protocol: config.ProtocolGemini, model: "test-model"}
	wire := `data: {"candidates":[{"index":0,"content":{"parts":[{"text":"a"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":2},"modelVersion":"test-model"}` + "\n\n" +
		`data: {"candidates":[{"index":0,"content":{"parts":[{"text":"b"}]}}],"modelVersion":"mutated"}` + "\n\n"
	u := newUsageStream("text/event-stream", "", p)
	_, _ = io.WriteString(u, wire)
	u.Close()
	if !u.finalSeen || u.tokens.InputTokens != 7 || u.tokens.OutputTokens != 2 || u.tokens.status != usageStatusComplete {
		t.Fatalf("final usage changed: %+v", u.tokens)
	}
	if u.finalModel != "test-model" || u.model != "test-model" {
		t.Fatalf("tail model leaked: final=%q model=%q", u.finalModel, u.model)
	}
}
