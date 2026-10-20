package admin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

func TestQuotaSyncUngroupedProjectAliases(t *testing.T) {
	for _, group := range []string{"", "未分组"} {
		t.Run(group, func(t *testing.T) {
			h, stores, _ := newNamedTargetUpdateTestHandler(t, t.TempDir(), "target", []string{"mock-only"})
			for _, name := range []string{"source", "ungrouped"} {
				if err := stores.clientStore.Create(config.Client{Name: name, AccessKey: "local-test-" + name}); err != nil {
					t.Fatal(err)
				}
			}
			request := httptest.NewRequest(http.MethodPost, "/data/quota/source/sync", strings.NewReader(`{"project_group":"`+group+`"}`))
			response := httptest.NewRecorder()
			h.ServeHTTP(response, request)
			if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"ungrouped"`) {
				t.Fatalf("ungrouped project sync: %d %s", response.Code, response.Body.String())
			}
		})
	}
}
