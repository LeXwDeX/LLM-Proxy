package proxy

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

// Recovery probes must use the same target credential and declared URL strategy
// as ordinary requests; only HTTP 200 revives an exhausted key.
func TestRecoveryProbeProviderCredentialsAndPaths(t *testing.T) {
	tests := []struct{ endpoint, authMode, path, header, credential string }{
		{config.EndpointTypeOpenAI, "", "/v1/models", "Authorization", "Bearer probe-only"},
		{config.EndpointTypeAzureOpenAI, "", "/", "api-key", "probe-only"},
		{config.EndpointTypeClaude, "", "/v1/models", "x-api-key", "probe-only"},
		{config.EndpointTypeClaude, config.AuthModeBearer, "/v1/models", "Authorization", "Bearer probe-only"},
		{config.EndpointTypeGemini, "", "/v1beta/models", "x-goog-api-key", "probe-only"},
		{config.EndpointTypeOpenAIImage, "", "/v1/models", "Authorization", "Bearer probe-only"},
		{config.EndpointTypeDualProtocol, "", "/compat/v1/models", "Authorization", "Bearer probe-only"},
		{"unknown", "", "/v1/models", "Authorization", "Bearer probe-only"},
	}
	for _, tt := range tests {
		t.Run(tt.endpoint+tt.authMode, func(t *testing.T) {
			var status atomic.Int32
			status.Store(http.StatusOK)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				if request.Method != http.MethodGet || request.URL.Path != tt.path {
					t.Errorf("probe request: %s %s", request.Method, request.URL.Path)
				}
				if request.Header.Get(tt.header) != tt.credential {
					t.Errorf("probe omitted expected %s credential", tt.header)
				}
				for _, header := range []string{"Authorization", "api-key", "x-api-key", "x-goog-api-key"} {
					if header != tt.header && request.Header.Get(header) != "" {
						t.Errorf("probe leaked another credential header: %s", header)
					}
				}
				w.WriteHeader(int(status.Load()))
			}))
			defer server.Close()
			endpoint, _ := url.Parse(server.URL)
			target := &Target{Name: "probe", EndpointType: tt.endpoint, Endpoint: endpoint, AuthMode: tt.authMode, APIKeys: []string{"probe-only", "unused-probe-key"}, OpenAIPrefix: "/compat"}
			service := &Service{providerRegistry: DefaultProviderRegistry(), httpClient: server.Client()}
			state := newTargetState(target, slog.Default())
			if !service.probeKey(context.Background(), state, 0) {
				t.Fatal("HTTP 200 probe failed")
			}
			for _, code := range []int{http.StatusNoContent, http.StatusUnauthorized, http.StatusNotFound, http.StatusMethodNotAllowed} {
				status.Store(int32(code))
				if service.probeKey(context.Background(), state, 0) {
					t.Errorf("HTTP %d incorrectly revived a key", code)
				}
			}
		})
	}
}
