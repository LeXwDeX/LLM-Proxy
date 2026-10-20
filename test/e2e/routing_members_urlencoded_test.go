//go:build e2e

package e2e

import (
	"strings"
	"testing"
)

var urlEncodedHeader = map[string]string{"Content-Type": "application/x-www-form-urlencoded"}

// urlencoded forms are forwarded as is: duplicate, case-variant and
// percent-encoded protected keys reach the upstream as only the pair the
// proxy authorized, and ACL reads that same pair.
func TestRoutingMembersNormalizedURLEncoded(t *testing.T) {
	name := target(t, map[string]any{"endpoint_type": "openai", "endpoint": w.urlA, "api_key": "sk-up-urlenc",
		"model_mappings": []any{map[string]any{"upstream": "e2e-url-model"}}})
	_, key := client(t, []string{name}, nil)
	for _, form := range []string{
		"model=gpt-secret&messages=hi&model=e2e-url-model",
		"model=e2e-url-model&Model=gpt-secret",
		"MODEL=gpt-secret&model=e2e-url-model&n=1&N=9&n=2",
		"mod%65l=gpt-secret&model=e2e-url-model&Mod%65l=gpt-secret",
	} {
		seq := w.fakeA.LastSeq()
		resp, body := call(t, key, "POST", "/v1/chat/completions", form, urlEncodedHeader)
		if resp.StatusCode != 200 {
			t.Fatalf("%q: status %d: %s", form, resp.StatusCode, body)
		}
		rec := lastNewRecord(t, w.fakeA, seq)
		upstreamMember(t, rec, "model", "e2e-url-model")
		if strings.Contains(form, "N=9") {
			upstreamMember(t, rec, "n", "2")
		} else {
			upstreamMember(t, rec, "n", "")
		}
	}
	for _, form := range []string{
		"model=e2e-url-model&model=gpt-secret",
		"Model=e2e-url-model&model=gpt-secret",
		"model=e2e-url-model&mod%65l=gpt-secret",
		"Model=e2e-url-model",
	} {
		seq := w.fakeA.LastSeq()
		resp, body := call(t, key, "POST", "/v1/chat/completions", form, urlEncodedHeader)
		if resp.StatusCode < 400 || resp.StatusCode >= 500 {
			t.Fatalf("unauthorized form %q accepted: %d %s", form, resp.StatusCode, body)
		}
		if recs := newRecords(w.fakeA, seq); len(recs) != 0 {
			t.Fatalf("rejected form %q reached upstream: %+v", form, recs)
		}
	}
}
