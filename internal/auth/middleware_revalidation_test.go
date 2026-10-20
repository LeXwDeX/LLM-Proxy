package auth

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

func TestMiddlewareRevalidatesLongLivedRequest(t *testing.T) {
	for _, test := range []struct {
		name    string
		clients []config.Client
		status  int
	}{
		{"ACL refresh", []config.Client{{Name: "client", AccessKey: "token", AllowedTargets: []string{"replacement"}}}, 0},
		{"revoked key", nil, http.StatusUnauthorized},
		{"IP denied", []config.Client{{Name: "client", AccessKey: "token", IPAddress: "10.0.0.1"}}, http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := NewStore()
			if err := store.LoadFromConfig([]config.Client{{Name: "client", AccessKey: "token", AllowedTargets: []string{"old-target"}}}); err != nil {
				t.Fatal(err)
			}
			var active *http.Request
			handler := Middleware(store, slog.New(slog.NewTextHandler(io.Discard, nil)))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { active = r }))
			request := httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
			request.Header.Set("Authorization", "Bearer token")
			request.RemoteAddr = "127.0.0.1:1234"
			handler.ServeHTTP(httptest.NewRecorder(), request)
			if active == nil {
				t.Fatal("initial authentication failed")
			}
			if err := store.LoadFromConfig(test.clients); err != nil {
				t.Fatal(err)
			}
			principal, status := RevalidateRequest(active)
			if status != test.status {
				t.Fatalf("status=%d want=%d", status, test.status)
			}
			if status == 0 && (!principal.CanAccess("replacement") || principal.CanAccess("old-target")) {
				t.Fatal("stale ACL retained")
			}
		})
	}
}
