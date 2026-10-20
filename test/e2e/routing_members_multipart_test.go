//go:build e2e

package e2e

import (
	"strings"
	"testing"
)

const e2eBoundary = "e2e-norm-boundary"

// e2eForm writes a multipart body exactly in the given order, duplicates and
// case variants included, plus one image file part.
func e2eForm(fields ...[2]string) string {
	var b strings.Builder
	b.WriteString("--" + e2eBoundary + "\r\nContent-Disposition: form-data; name=\"image\"; filename=\"a.png\"\r\nContent-Type: image/png\r\n\r\n\x89PNG-fixture\r\n")
	for _, field := range fields {
		b.WriteString("--" + e2eBoundary + "\r\nContent-Disposition: form-data; name=\"" + field[0] + "\"\r\n\r\n" + field[1] + "\r\n")
	}
	b.WriteString("--" + e2eBoundary + "--\r\n")
	return b.String()
}

var multipartHeader = map[string]string{"Content-Type": "multipart/form-data; boundary=" + e2eBoundary}

// Multipart forms with duplicate or case-variant protected fields reach the
// upstream with only the field the proxy authorized, both when forwarded as
// multipart (openai_image) and when converted to JSON (openai).
func TestRoutingMembersNormalizedMultipart(t *testing.T) {
	passthrough := target(t, map[string]any{"endpoint_type": "openai_image", "endpoint": w.urlA + "/openai/v1/images/edits",
		"image_operation": "edits", "api_key": "sk-up-form",
		"model_mappings": []any{map[string]any{"upstream": "gpt-image-norm", "fallback": "e2e-form-pass"}}})
	converted := target(t, map[string]any{"endpoint_type": "openai", "endpoint": w.urlA, "api_key": "sk-up-form",
		"model_mappings": []any{map[string]any{"upstream": "gpt-image-conv", "fallback": "e2e-form-conv"}}})
	_, passKey := client(t, []string{passthrough}, nil)
	_, convKey := client(t, []string{converted}, nil)
	prompt := [2]string{"prompt", "edit [[fake usage.in=5 usage.out=2]]"}
	for _, tc := range []struct {
		name, key, alias, model string
		multipart               bool
	}{
		{"passthrough", passKey, "e2e-form-pass", "gpt-image-norm", true},
		{"converted", convKey, "e2e-form-conv", `"gpt-image-conv"`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, form := range []string{
				e2eForm([2]string{"model", "gpt-secret"}, prompt, [2]string{"model", tc.alias}),
				e2eForm([2]string{"model", tc.alias}, [2]string{"Model", "gpt-secret"}, prompt),
				e2eForm([2]string{"MODEL", "gpt-secret"}, [2]string{"model", tc.alias}, [2]string{"n", "1"}, [2]string{"N", "9"}, [2]string{"n", "2"}, prompt),
			} {
				seq := w.fakeA.LastSeq()
				resp, body := call(t, tc.key, "POST", "/v1/images/edits", form, multipartHeader)
				if resp.StatusCode != 200 {
					t.Fatalf("status %d: %s", resp.StatusCode, body)
				}
				rec := lastNewRecord(t, w.fakeA, seq)
				upstreamMember(t, rec, "model", tc.model)
				if strings.Contains(form, "\"N\"") {
					// Multipart text "2" or the converted JSON number 2.
					upstreamMember(t, rec, "n", "2")
				} else {
					upstreamMember(t, rec, "n", "")
				}
			}
			// ACL reads the last exact model field, the one upstream would
			// get: an unauthorized model there, or only a variant, is refused
			// and never forwarded.
			for _, form := range []string{
				e2eForm([2]string{"model", tc.alias}, prompt, [2]string{"model", "gpt-secret"}),
				e2eForm([2]string{"Model", tc.alias}, [2]string{"model", "gpt-secret"}, prompt),
				e2eForm([2]string{"Model", tc.alias}, prompt),
			} {
				seq := w.fakeA.LastSeq()
				resp, body := call(t, tc.key, "POST", "/v1/images/edits", form, multipartHeader)
				if resp.StatusCode < 400 || resp.StatusCode >= 500 {
					t.Fatalf("unauthorized form accepted: %d %s", resp.StatusCode, body)
				}
				if recs := newRecords(w.fakeA, seq); len(recs) != 0 {
					t.Fatalf("rejected form reached upstream: %+v", recs)
				}
			}
		})
	}
}
