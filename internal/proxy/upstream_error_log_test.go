package proxy

import (
	"bytes"
	"compress/gzip"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/errorlog"
)

// #186: a parseable upstream error envelope records only controlled fields;
// the message text and the body bytes never reach the log.
func TestUpstreamErrorLogRecordsControlledEnvelope(t *testing.T) {
	path := filepath.Join(t.TempDir(), "upstream-error.log")
	errorlog.Init(path)
	t.Cleanup(func() { _ = errorlog.Close() })

	const marker = "RAW-UPSTREAM-MESSAGE-MUST-NOT-LEAK"
	body := `{"error":{"code":"invalid_function_parameters","type":"invalid_request_error","param":"tools[0].parameters","status":"400","message":"` + marker + `"}}`
	s, principal, _ := newTraceCaptureService(t, false, http.StatusBadRequest, "application/json; charset=utf-8", body)
	w := serveTraceCapture(s, principal)
	if w.Code != http.StatusBadRequest || w.Body.String() != body {
		t.Fatalf("forwarded status=%d body=%q", w.Code, w.Body.String())
	}

	entries := readErrorLogEntries(t, path)
	if len(entries) != 1 {
		t.Fatalf("error log entries = %d, want 1", len(entries))
	}
	entry := entries[0]
	if entry.RespErrorCode != "invalid_function_parameters" {
		t.Fatalf("resp_error_code = %q", entry.RespErrorCode)
	}
	if entry.RespErrorType != "invalid_request_error" {
		t.Fatalf("resp_error_type = %q", entry.RespErrorType)
	}
	if entry.RespErrorParam != "tools[0].parameters" {
		t.Fatalf("resp_error_param = %q", entry.RespErrorParam)
	}
	if entry.RespErrorStatus != "400" {
		t.Fatalf("resp_error_status = %q", entry.RespErrorStatus)
	}
	if entry.RespErrorParse != "" || entry.ContentType != "" {
		t.Fatalf("parsed envelope marked as failure: parse=%q content_type=%q", entry.RespErrorParse, entry.ContentType)
	}
	if entry.RespExcerpt != "[redacted]" || entry.RespBytes != len(body) {
		t.Fatalf("body contract changed: excerpt=%q bytes=%d", entry.RespExcerpt, entry.RespBytes)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{marker, `"model":"m"`, `"stream":true`} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("error log leaked %q: %s", forbidden, raw)
		}
	}
}

// #186: anything that is not the controlled {"error":{...}} envelope is
// reported as a parse failure with only the media type kept. It is never
// guessed from top-level code/message fields.
func TestUpstreamErrorLogMarksUnparseableEnvelope(t *testing.T) {
	path := filepath.Join(t.TempDir(), "upstream-error.log")
	errorlog.Init(path)
	t.Cleanup(func() { _ = errorlog.Close() })

	oversized := `{"error":{"code":"invalid_function_parameters","param":"` + strings.Repeat("p", maxCachedUpstreamErrorBody) + `"}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	target := &Target{Name: "t"}

	cases := []struct {
		name        string
		body        []byte
		contentType string
		encoding    string
		wantType    string
	}{
		{"non-json", []byte("upstream failed"), "text/plain; charset=utf-8", "", "text/plain"},
		{"top-level code only", []byte(`{"code":"invalid_function_parameters","message":"x"}`), "application/json; charset=utf-8", "", "application/json"},
		{"error is a string", []byte(`{"error":"boom"}`), "application/json", "", "application/json"},
		{"error is null", []byte(`{"error":null}`), "application/json", "", "application/json"},
		{"error is an array", []byte(`{"error":[{"code":"x"}]}`), "application/json", "", "application/json"},
		{"oversized body", []byte(oversized), "application/json", "", "application/json"},
		{"invalid gzip", []byte{0x1f, 0x8b, 0x08, 0x00, 0x00}, "application/json", "gzip", "application/json"},
	}
	for _, tc := range cases {
		header := http.Header{"Content-Type": {tc.contentType}}
		if tc.encoding != "" {
			header.Set("Content-Encoding", tc.encoding)
		}
		resp := &http.Response{StatusCode: http.StatusBadRequest, Header: header, Request: req}
		writeUpstreamErrorLog(req, target, resp, tc.body, time.Now())
	}

	entries := readErrorLogEntries(t, path)
	if len(entries) != len(cases) {
		t.Fatalf("error log entries = %d, want %d", len(entries), len(cases))
	}
	for i, tc := range cases {
		entry := entries[i]
		if entry.RespErrorParse != "failed" {
			t.Errorf("%s: resp_error_parse = %q, want failed", tc.name, entry.RespErrorParse)
		}
		if entry.RespErrorCode != "" || entry.RespErrorType != "" || entry.RespErrorParam != "" || entry.RespErrorStatus != "" {
			t.Errorf("%s: parsed fields on a rejected envelope: %+v", tc.name, entry)
		}
		if entry.ContentType != tc.wantType {
			t.Errorf("%s: content_type = %q, want %q", tc.name, entry.ContentType, tc.wantType)
		}
		if entry.RespExcerpt != "[redacted]" || entry.RespBytes != len(tc.body) {
			t.Errorf("%s: body contract changed: excerpt=%q bytes=%d", tc.name, entry.RespExcerpt, entry.RespBytes)
		}
	}
}

// #186: compression is decoded for diagnostics while the bytes forwarded to
// the client stay untouched.
func TestUpstreamErrorLogDecodesCompressedEnvelope(t *testing.T) {
	path := filepath.Join(t.TempDir(), "upstream-error.log")
	errorlog.Init(path)
	t.Cleanup(func() { _ = errorlog.Close() })

	var compressed bytes.Buffer
	zw := gzip.NewWriter(&compressed)
	if _, err := zw.Write([]byte(`{"error":{"code":"invalid_function_parameters","param":"tools[1].parameters"}}`)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	resp := &http.Response{
		StatusCode: http.StatusBadRequest,
		Header:     http.Header{"Content-Type": {"application/json"}, "Content-Encoding": {"gzip"}},
		Request:    req,
	}
	writeUpstreamErrorLog(req, &Target{Name: "t"}, resp, compressed.Bytes(), time.Now())

	entries := readErrorLogEntries(t, path)
	if len(entries) != 1 {
		t.Fatalf("error log entries = %d, want 1", len(entries))
	}
	entry := entries[0]
	if entry.RespErrorCode != "invalid_function_parameters" || entry.RespErrorParam != "tools[1].parameters" {
		t.Fatalf("decoded fields = %+v", entry)
	}
	if entry.RespErrorParse != "" {
		t.Fatalf("resp_error_parse = %q, want empty", entry.RespErrorParse)
	}
}

// #187: a deterministic client error stops before cross-target failover; the
// client sees the original response and no dequeue counter moves.
func TestDeterministicClientErrorSkipsFailover(t *testing.T) {
	var logs bytes.Buffer
	s, principal, _ := newTraceCaptureService(t, false, http.StatusBadRequest, "application/json",
		`{"error":{"code":"invalid_function_parameters","type":"invalid_request_error","param":"tools[0].parameters","message":"from-a"}}`)
	s.logger = slog.New(slog.NewJSONHandler(&logs, nil))

	w := serveTraceCapture(s, principal)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if got := w.Body.String(); !strings.Contains(got, `"invalid_function_parameters"`) {
		t.Fatalf("client body = %q, want the original upstream error", got)
	}
	if got := s.metrics.totalDequeued.Load(); got != 0 {
		t.Fatalf("totalDequeued = %d, want 0", got)
	}
	if got := s.metrics.totalDequeue4xx.Load(); got != 0 {
		t.Fatalf("totalDequeue4xx = %d, want 0", got)
	}
	if got := s.metrics.totalTargetRetries.Load(); got != 0 {
		t.Fatalf("totalTargetRetries = %d, want 0", got)
	}

	logged := logs.String()
	for _, want := range []string{
		`"msg":"failover skipped for deterministic client error"`,
		`"failover":"skipped"`,
		`"reason":"deterministic_client_error"`,
		`"code":"invalid_function_parameters"`,
		`"status":400`,
	} {
		if !strings.Contains(logged, want) {
			t.Fatalf("log missing %s: %s", want, logged)
		}
	}
}

// #187 counterexample: a non-whitelisted 4xx still crosses targets.
func TestNonDeterministicClientErrorStillDequeues(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"target-specific code", `{"error":{"code":"bad_a","message":"from-a"}}`},
		{"unparseable body", `not json`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, principal, _ := newTraceCaptureService(t, false, http.StatusBadRequest, "application/json", tc.body)
			w := serveTraceCapture(s, principal)
			if w.Code != http.StatusBadRequest || w.Body.String() != tc.body {
				t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
			}
			if got := s.metrics.totalDequeued.Load(); got != 1 {
				t.Fatalf("totalDequeued = %d, want 1", got)
			}
			if got := s.metrics.totalDequeue4xx.Load(); got != 1 {
				t.Fatalf("totalDequeue4xx = %d, want 1", got)
			}
		})
	}
}
