package proxy

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

func TestAutomaticGzipNegotiationCompatibility(t *testing.T) {
	for _, tc := range []struct {
		name, method, accept, rangeHeader, transport string
		decoded                                      bool
	}{
		{"owned_default", http.MethodGet, "", "", "owned", true},
		{"explicit_client", http.MethodGet, "gzip", "", "owned", false},
		{"range", http.MethodGet, "", "bytes=0-100", "owned", false},
		{"head", http.MethodHead, "", "", "owned", false},
		{"custom_disabled", http.MethodGet, "", "", "disabled", false},
		{"custom_enabled", http.MethodGet, "", "", "enabled", true},
		{"default_transport", http.MethodGet, "", "", "default", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plain := []byte(`{"usage":{"prompt_tokens":10,"completion_tokens":2}}`)
			wire := observationFixture(t, "gzip", plain)
			var received atomic.Value
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				received.Store(r.Header.Get("Accept-Encoding"))
				w.Header().Set("Content-Encoding", "gzip")
				_, _ = w.Write(wire)
			}))
			defer upstream.Close()
			svc, err := NewService(&config.Config{}, newTestLogger())
			if err != nil {
				t.Fatal(err)
			}
			defer svc.Close()
			if tc.transport == "disabled" || tc.transport == "enabled" {
				custom := svc.automaticGzipTransport.Clone()
				custom.DisableCompression = tc.transport == "disabled"
				svc.httpClient.Transport = custom
				defer custom.CloseIdleConnections()
			}
			if tc.transport == "default" {
				svc.httpClient.Transport = nil
			}
			req, _ := http.NewRequest(tc.method, upstream.URL, nil)
			if tc.accept != "" {
				req.Header.Set("Accept-Encoding", tc.accept)
			}
			if tc.rangeHeader != "" {
				req.Header.Set("Range", tc.rangeHeader)
			}
			response, err := svc.doHTTP(req)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			data, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			want := wire
			if tc.decoded {
				want = plain
			}
			if tc.method == http.MethodHead {
				want = nil
			}
			if !bytes.Equal(data, want) || response.Uncompressed != tc.decoded {
				t.Fatalf("body behavior changed: decoded=%v metadata=%v", tc.decoded, response)
			}
			wantAccept := tc.accept
			if tc.decoded {
				wantAccept = "gzip"
			}
			if received.Load().(string) != wantAccept {
				t.Fatalf("negotiation=%q want %q", received.Load().(string), wantAccept)
			}
			if tc.decoded && (response.Header.Get("Content-Encoding") != "" || response.Header.Get("Content-Length") != "" || response.ContentLength != -1) {
				t.Fatalf("decoded metadata inconsistent: %v", response)
			}
		})
	}
}

func TestAutomaticGzipFailureRemainsPostSend(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = io.WriteString(w, "broken gzip")
	}))
	defer upstream.Close()
	svc, err := NewService(&config.Config{}, newTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	req, _ := http.NewRequest(http.MethodGet, upstream.URL, nil)
	response, err := svc.doHTTP(req)
	if err != nil || response == nil {
		t.Fatalf("post-send failure became transport error: %v", err)
	}
	defer response.Body.Close()
	if _, err := io.ReadAll(response.Body); err == nil || calls.Load() != 1 {
		t.Fatalf("bad gzip accepted/replayed: error=%v calls=%d", err, calls.Load())
	}
}

// net/http preserves bidirectional upgraded/tunnel bodies without gzip wrapping.
type automaticGzipWritableBody struct{ bytes.Buffer }

func (*automaticGzipWritableBody) Close() error { return nil }

func TestAutomaticGzipPreservesUpgradesAndEmptyBodies(t *testing.T) {
	t.Run("writable_upgrade", func(t *testing.T) {
		body := &automaticGzipWritableBody{}
		response := &http.Response{StatusCode: 101, Header: http.Header{"Content-Encoding": {"gzip"}, "Content-Length": {"12"}}, ContentLength: -1, Body: body}
		normalizeAutomaticGzip(response, context.Background())
		if response.Body != body || response.Uncompressed || response.Header.Get("Content-Encoding") != "gzip" || response.Header.Get("Content-Length") != "12" || response.ContentLength != -1 {
			t.Fatalf("writable body or metadata changed: %+v", response)
		}
		if _, ok := response.Body.(io.Writer); !ok {
			t.Fatal("upgrade body lost writer")
		}
	})
	t.Run("zero_length", func(t *testing.T) {
		response := &http.Response{StatusCode: 204, Header: http.Header{"Content-Encoding": {"gzip"}, "Content-Length": {"0"}}, ContentLength: 0, Body: http.NoBody}
		normalizeAutomaticGzip(response, context.Background())
		n, err := response.Body.Read(make([]byte, 1))
		if response.Body != http.NoBody || response.Uncompressed || response.Header.Get("Content-Encoding") != "gzip" || response.Header.Get("Content-Length") != "0" || response.ContentLength != 0 || n != 0 || err != io.EOF {
			t.Fatalf("empty response changed EOF or metadata: %+v n=%d err=%v", response, n, err)
		}
	})
}
