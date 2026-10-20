package proxy

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

func serveWithContentEncoding(s *Service, principal *auth.Principal, path string, encodings []string, body []byte) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	for _, encoding := range encodings {
		r.Header.Add("Content-Encoding", encoding)
	}
	r = r.WithContext(auth.WithPrincipal(r.Context(), principal))
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

// L1: the proxy does not decode request bodies, so an encoded body would be
// forwarded without the proxy reading its model or normalizing it. It is
// refused with 415 unsupported_content_encoding and never forwarded.
func TestRequestContentEncodingRejected(t *testing.T) {
	s, captured, principal := newNormalizeHTTPService(t, []config.ModelMapping{{Upstream: "test-model"}})
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write([]byte(`{"model":"gpt-secret","messages":[]}`))
	_ = zw.Close()
	plain := []byte(`{"model":"test-model","messages":[]}`)
	for _, tc := range []struct {
		name      string
		encodings []string
		body      []byte
	}{
		{"gzip", []string{"gzip"}, gz.Bytes()},
		{"gzip case", []string{"GZip"}, gz.Bytes()},
		{"br label on plain body", []string{"br"}, plain},
		{"unknown", []string{"x-custom"}, plain},
		{"stacked", []string{"identity, gzip"}, gz.Bytes()},
		{"multiple fields", []string{"identity", "gzip"}, gz.Bytes()},
		{"empty member", []string{"identity,,deflate"}, plain},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := captured.count()
			// Gemini and Azure deployments carry the model in the path: the
			// body is still never forwarded unread.
			for _, path := range []string{"/v1/chat/completions", "/v1beta/models/test-model:generateContent"} {
				w := serveWithContentEncoding(s, principal, path, tc.encodings, tc.body)
				var response requestError
				if w.Code != http.StatusUnsupportedMediaType || json.Unmarshal(w.Body.Bytes(), &response) != nil || response.Error.Code != "unsupported_content_encoding" || response.Error.Type != "invalid_request_error" {
					t.Fatalf("%s: status=%d body=%s, want 415 unsupported_content_encoding", path, w.Code, w.Body.String())
				}
				if got := w.Header().Get("Accept-Encoding"); got != "identity" {
					t.Fatalf("%s: Accept-Encoding %q, want identity", path, got)
				}
			}
			if captured.count() != before {
				t.Fatal("encoded body forwarded")
			}
		})
	}
	// identity (any case, padded) and an empty body stay accepted.
	for _, encodings := range [][]string{{"identity"}, {" Identity "}, {""}, {"identity", "IDENTITY"}} {
		if w := serveWithContentEncoding(s, principal, "/v1/chat/completions", encodings, plain); w.Code != http.StatusOK {
			t.Fatalf("%q: status=%d body=%s", encodings, w.Code, w.Body.String())
		}
	}
	if w := serveWithContentEncoding(s, principal, "/v1/chat/completions", []string{"gzip"}, nil); w.Code == http.StatusUnsupportedMediaType {
		t.Fatalf("empty body refused: %s", w.Body.String())
	}
}
