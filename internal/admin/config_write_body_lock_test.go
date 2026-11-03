package admin

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Issue #151: write handlers must read and decode the (possibly slow or huge)
// body before taking configMu, and cap its size.
func TestConfigWriteHandlersDoNotHoldConfigMuWhileReadingBody(t *testing.T) {
	h, _ := newQuotaSyncTestHandler(t)
	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/data/clients"},
		{http.MethodPut, "/data/clients/x"},
		{http.MethodPost, "/data/pools"},
		{http.MethodPut, "/data/pools/x"},
		{http.MethodPost, "/data/targets"},
		{http.MethodPut, "/data/targets/x"},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			pr, pw := io.Pipe()
			req := httptest.NewRequest(tc.method, tc.path, pr)
			rec := httptest.NewRecorder()
			done := make(chan struct{})
			go func() { h.ServeHTTP(rec, req); close(done) }()
			// Give the handler time to block reading the body.
			time.Sleep(50 * time.Millisecond)
			locked := !h.configMu.TryLock()
			if !locked {
				h.configMu.Unlock()
			}
			pw.Write([]byte("{not json"))
			pw.Close()
			<-done
			if locked {
				t.Fatal("configMu held while the request body was still being read")
			}
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestConfigWriteHandlersCapBodySize(t *testing.T) {
	h, _ := newQuotaSyncTestHandler(t)
	huge := `{"name":"` + strings.Repeat("a", maxConfigWriteBody+1024) + `"}`
	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/data/clients"},
		{http.MethodPost, "/data/pools"},
		{http.MethodPost, "/data/targets"},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, strings.NewReader(huge)))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status %d", tc.path, rec.Code)
		}
	}
}
