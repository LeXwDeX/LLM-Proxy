//go:build e2e

package e2e

import (
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/fakellm"
)

// A sent attempt that ended before response headers (fakellm drops the
// connection after reading the request) fails over to a target answering a
// non-2xx: the client gets that response, which is never billed, and the
// request still records exactly one zero-token incomplete event for the sent
// attempt. Without a sent attempt a non-2xx response records nothing.
func TestSentAttemptThenNon2xxRecordsZeroEvent(t *testing.T) {
	p := pool(t)
	alias := uniq("e2e-pend")
	kSent, kReject := uniq("sk-sent"), uniq("sk-reject")
	sentTarget := keyedTarget(t, w.urlA, "/openai/v1", alias, []string{kSent}, map[string]any{"pool": p})
	keyedTarget(t, w.urlB, "/openai/v1", alias, []string{kReject}, map[string]any{"pool": p})
	w.fakeA.SetKeyScenario(kSent, &fakellm.Scenario{Abort: true})
	defer w.fakeA.SetKeyScenario(kSent, nil)
	w.fakeB.SetKeyScenario(kReject, &fakellm.Scenario{Preset: "bad_request"})
	defer w.fakeB.SetKeyScenario(kReject, nil)

	// Pool selection is random on first use: take fresh clients until one is
	// first routed to the aborting target.
	sentClient := ""
	for attempt := 0; attempt < 16 && sentClient == ""; attempt++ {
		name, key := client(t, nil, []string{p})
		fromA, fromB := w.fakeA.LastSeq(), w.fakeB.LastSeq()
		resp, body := chat(t, key, alias, "hi")
		if resp.StatusCode != 400 || !strings.Contains(string(body), "fake invalid request") {
			t.Fatalf("want the passed-through 400: %d %s", resp.StatusCode, body)
		}
		if hits(w.fakeB, kReject, fromB) != 1 {
			t.Fatalf("rejecting target not reached exactly once")
		}
		if hits(w.fakeA, kSent, fromA) == 1 {
			sentClient = name
		}
	}
	if sentClient == "" {
		t.Fatal("no fresh client was routed to the aborting target first in 16 attempts")
	}
	events := usageFor(t, sentClient, 1)
	time.Sleep(300 * time.Millisecond)
	if events = usageFor(t, sentClient, 1); len(events) != 1 {
		t.Fatalf("usage events=%d want exactly 1: %+v", len(events), events)
	}
	ev := events[0]
	if ev.Target != sentTarget || ev.InputTokens != 0 || ev.OutputTokens != 0 || ev.UsageStatus != "incomplete" || !ev.CostIncomplete ||
		!strings.Contains(ev.UsageDiagnostic, "upstream_interrupted_no_usage") {
		t.Fatalf("want the sent attempt's zero-token incomplete event: %+v", ev)
	}

	// Counter-example: a client whose only target answers 400 directly has
	// no sent attempt without usage, so nothing is recorded; a later success
	// is its only event.
	directAlias, kDirect := uniq("e2e-pend-direct"), uniq("sk-direct")
	direct := keyedTarget(t, w.urlB, "/openai/v1", directAlias, []string{kDirect}, nil)
	w.fakeB.SetKeyScenario(kDirect, &fakellm.Scenario{Preset: "bad_request"})
	defer w.fakeB.SetKeyScenario(kDirect, nil)
	name, key := client(t, []string{direct}, nil)
	if resp, body := chat(t, key, directAlias, "hi"); resp.StatusCode != 400 {
		t.Fatalf("direct: want 400: %d %s", resp.StatusCode, body)
	}
	w.fakeB.SetKeyScenario(kDirect, nil)
	if resp, body := chat(t, key, directAlias, "hi"); resp.StatusCode != 200 {
		t.Fatalf("direct success: %d %s", resp.StatusCode, body)
	}
	usageFor(t, name, 1)
	time.Sleep(300 * time.Millisecond)
	if events := usageFor(t, name, 1); len(events) != 1 || events[0].StatusCode != 200 || events[0].UsageStatus != "complete" {
		t.Fatalf("direct 400 recorded usage: %+v", events)
	}
}
