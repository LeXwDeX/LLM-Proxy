//go:build e2e

package e2e

import "testing"

// coversUsageWants backs usageForWant: it must treat wants as a multiset and
// not let a wildcard want consume the event a specific want needs.
func TestCoversUsageWants(t *testing.T) {
	chat := usageEvent{Path: "/v1/chat/completions", StatusCode: 200}
	count := usageEvent{Path: "/v1/messages/count_tokens", StatusCode: 200}
	msg := usageEvent{Path: "/v1/messages", StatusCode: 200}
	for _, tc := range []struct {
		name   string
		events []usageEvent
		wants  []usageWant
		ok     bool
	}{
		{"unrelated event does not satisfy", []usageEvent{chat, count}, []usageWant{{Path: "/v1/chat/completions"}, {Path: "/v1/messages"}}, false},
		{"all paths present", []usageEvent{chat, count, msg}, []usageWant{{Path: "/v1/chat/completions"}, {Path: "/v1/messages"}}, true},
		{"multiset needs two events", []usageEvent{chat}, []usageWant{{Path: "/v1/chat/completions"}, {Path: "/v1/chat/completions"}}, false},
		{"multiset with two events", []usageEvent{chat, chat}, []usageWant{{Path: "/v1/chat/completions"}, {Path: "/v1/chat/completions"}}, true},
		{"wildcard listed first cannot steal", []usageEvent{chat, msg}, []usageWant{{}, {Path: "/v1/chat/completions"}}, true},
		{"wildcard needs a spare event", []usageEvent{chat}, []usageWant{{}, {Path: "/v1/chat/completions"}}, false},
		{"status filter", []usageEvent{{Path: "/x", StatusCode: 500}}, []usageWant{{StatusCode: 200}}, false},
		{"no wants", nil, nil, true},
	} {
		if got := coversUsageWants(tc.events, tc.wants); got != tc.ok {
			t.Errorf("%s: coversUsageWants = %v, want %v", tc.name, got, tc.ok)
		}
	}
}
