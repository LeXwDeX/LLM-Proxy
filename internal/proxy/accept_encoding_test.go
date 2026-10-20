package proxy

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

func TestWithoutBrotliAcceptEncoding(t *testing.T) {
	for _, tc := range []struct {
		in      []string
		want    string
		changed bool
	}{
		{[]string{"br"}, "identity", true},
		{[]string{"BR"}, "identity", true},
		{[]string{"br, gzip"}, "gzip", true},
		{[]string{"gzip", "br"}, "gzip", true},
		{[]string{"gzip;q=1.0, br;q=0.9, deflate;q=0.5"}, "gzip;q=1.0, deflate;q=0.5", true},
		{[]string{" br ; q=1 ,zstd"}, "zstd", true},
		{[]string{"*"}, "*, br;q=0", true},
		{[]string{"*;q=0.5, gzip"}, "*;q=0.5, gzip, br;q=0", true},
		{[]string{"br;q=0, *"}, "*, br;q=0", true},
		{[]string{"gzip, deflate"}, "", false},
		{[]string{"identity"}, "", false},
		{[]string{"brotli, gzip"}, "", false},
		{nil, "", false},
		{[]string{""}, "", false},
	} {
		got, changed := withoutBrotliAcceptEncoding(tc.in)
		if got != tc.want || changed != tc.changed {
			t.Fatalf("%q: got %q changed=%v, want %q %v", tc.in, got, changed, tc.want, tc.changed)
		}
	}
}

// A client advertising br could exhaust the shared br observation budget and
// have its streams settled with zero tokens. Requests whose usage is observed
// therefore never offer br upstream; the remaining codings, q-values and the
// proxy's own gzip negotiation stay as they were.
func TestUpstreamAcceptEncodingWithoutBrotli(t *testing.T) {
	const payload = `{"choices":[{"index":0,"message":{"content":"x"},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":7}}`
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write([]byte(payload))
	_ = zw.Close()
	var mu sync.Mutex
	var seen []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		mu.Lock()
		seen = append(seen, strings.Join(r.Header.Values("Accept-Encoding"), "|"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			w.Header().Set("Content-Encoding", "gzip")
			_, _ = w.Write(gz.Bytes())
			return
		}
		_, _ = w.Write([]byte(payload))
	}))
	defer upstream.Close()
	newService := func(t *testing.T) (*Service, *goneEventRecorder) {
		s, err := NewService(&config.Config{Server: config.ServerConfig{RequestTimeoutSeconds: 10}, Targets: []config.Target{{Name: "t", EndpointType: config.EndpointTypeOpenAI, Endpoint: upstream.URL, UsageAdapter: "standard", APIKey: "k0", ModelMappings: []config.ModelMapping{{Upstream: "test-model"}}}}}, newTestLogger())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Close() })
		events := &goneEventRecorder{}
		s.SetUsageRecorder(events)
		return s, events
	}
	serve := func(s *Service, acceptEncoding []string) (*httptest.ResponseRecorder, string) {
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"test-model","messages":[]}`))
		r.Header.Set("Content-Type", "application/json")
		for _, value := range acceptEncoding {
			r.Header.Add("Accept-Encoding", value)
		}
		r = r.WithContext(auth.WithPrincipal(r.Context(), newAllowAllPrincipal(t, "accept-encoding")))
		w := httptest.NewRecorder()
		mu.Lock()
		before := len(seen)
		mu.Unlock()
		s.ServeHTTP(w, r)
		mu.Lock()
		defer mu.Unlock()
		if len(seen) != before+1 {
			t.Fatalf("%q: upstream requests %d", acceptEncoding, len(seen)-before)
		}
		return w, seen[len(seen)-1]
	}
	for _, tc := range []struct {
		client          []string
		upstream        string
		gzippedToClient bool
	}{
		{[]string{"br, gzip"}, "gzip", true},
		{[]string{"br"}, "identity", false},
		{[]string{"gzip;q=1.0, br;q=0.9, deflate;q=0.5"}, "gzip;q=1.0, deflate;q=0.5", true},
		{[]string{"gzip", "br"}, "gzip", true},
		{[]string{"*"}, "*, br;q=0", false},
		{[]string{"gzip, deflate"}, "gzip, deflate", true},
		// No client preference: the proxy's own gzip negotiation, decoded.
		{nil, "gzip", false},
	} {
		s, events := newService(t)
		w, upstreamAE := serve(s, tc.client)
		if upstreamAE != tc.upstream {
			t.Fatalf("%q: upstream Accept-Encoding %q, want %q", tc.client, upstreamAE, tc.upstream)
		}
		if w.Code != http.StatusOK {
			t.Fatalf("%q: status %d", tc.client, w.Code)
		}
		if tc.gzippedToClient {
			if w.Header().Get("Content-Encoding") != "gzip" || !bytes.Equal(w.Body.Bytes(), gz.Bytes()) {
				t.Fatalf("%q: gzip bytes changed: encoding=%q", tc.client, w.Header().Get("Content-Encoding"))
			}
		} else if w.Header().Get("Content-Encoding") != "" || w.Body.String() != payload {
			t.Fatalf("%q: identity body changed: encoding=%q body=%q", tc.client, w.Header().Get("Content-Encoding"), w.Body.String())
		}
		got := events.snapshot()
		if len(got) != 1 || got[0].InputTokens != 11 || got[0].OutputTokens != 7 || got[0].UsageStatus != "complete" {
			t.Fatalf("%q: usage events %+v", tc.client, got)
		}
	}
	// Nothing observes usage: the client's preference passes through as is.
	s, _ := newService(t)
	s.SetUsageRecorder(nil)
	traces := s.traceStore
	s.traceStore = nil
	_ = traces.Close()
	if _, upstreamAE := serve(s, []string{"br, gzip"}); upstreamAE != "br, gzip" {
		t.Fatalf("unobserved request: upstream Accept-Encoding %q", upstreamAE)
	}
}
