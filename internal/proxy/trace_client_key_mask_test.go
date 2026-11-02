package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	appmiddleware "github.com/LeXwDeX/LLM-Proxy/internal/middleware"
)

// Trace records persist the client access key; only the last four characters
// of a long key may survive, and short keys must be fully hidden.
func TestTraceRecordMasksClientAccessKeySuffixOnly(t *testing.T) {
	service, _, store := newTraceCaptureService(t, true, http.StatusOK, "application/json", `{}`)

	cases := []struct {
		key  string
		want string
	}{
		{"sk-client-long-secret-1234567890", "****7890"},
		{"trace-token", "****"},
	}
	clients := make([]config.Client, 0, len(cases))
	for i, tc := range cases {
		clients = append(clients, config.Client{Name: "client-" + string(rune('a'+i)), AccessKey: tc.key})
	}
	authStore := auth.NewStore()
	if err := authStore.LoadFromConfig(clients); err != nil {
		t.Fatal(err)
	}

	for i, tc := range cases {
		principal, ok := authStore.Authenticate(tc.key)
		if !ok {
			t.Fatalf("authenticate %q failed", tc.key)
		}
		traceID := "trace-" + string(rune('a'+i))
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		req.Header.Set(appmiddleware.HeaderRequestID, traceID)
		req = req.WithContext(auth.WithPrincipal(req.Context(), principal))
		rec := httptest.NewRecorder()
		appmiddleware.RequestID()(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Request: r}
			service.recordTrace(r, &Target{Name: "t", EndpointType: config.EndpointTypeOpenAI}, resp, []byte(`{}`), []byte(`{}`), time.Now(), -1, "m", usageTokens{})
		})).ServeHTTP(rec, req)

		record := store.Get(traceID)
		if record == nil {
			t.Fatalf("trace %q for %q was not recorded", traceID, tc.key)
		}
		if record.ClientAccessKey != tc.want {
			t.Errorf("client_access_key = %q, want %q", record.ClientAccessKey, tc.want)
		}
		if strings.Contains(record.ClientAccessKey, tc.key) {
			t.Errorf("client_access_key %q contains the full key", record.ClientAccessKey)
		}
	}
}
