package middleware

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestIssue74AccessLogOmitsAllQueryValues(t *testing.T) {
	var logs bytes.Buffer
	handler := AccessLogger(slog.New(slog.NewJSONHandler(&logs, nil)))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "api_key=fixture-one&arbitrary=fixture-two" {
			t.Errorf("request query changed: %s", r.URL.RawQuery)
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	request := httptest.NewRequest(http.MethodGet, "/v1/models?api_key=fixture-one&arbitrary=fixture-two", nil)
	handler.ServeHTTP(httptest.NewRecorder(), request)
	if strings.Contains(logs.String(), "fixture-one") || strings.Contains(logs.String(), "fixture-two") || !strings.Contains(logs.String(), "/v1/models") {
		t.Errorf("unsafe access log: %s", logs.String())
	}
}

func TestIssue74RecoveredPanicDoesNotLogRawValue(t *testing.T) {
	var logs bytes.Buffer
	handler := Recoverer(slog.New(slog.NewJSONHandler(&logs, nil)))(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("fixture-panic-body")
	}))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code != http.StatusInternalServerError || strings.Contains(logs.String(), "fixture-panic-body") || !strings.Contains(logs.String(), "panic recovered") {
		t.Errorf("status=%d log=%s", rec.Code, logs.String())
	}
}
