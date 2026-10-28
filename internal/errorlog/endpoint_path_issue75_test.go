package errorlog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIssue75ErrorLogDropsEndpointPaths(t *testing.T) {
	path := filepath.Join(t.TempDir(), "errors.log")
	Init(path)
	t.Cleanup(func() { _ = Close() })
	Write(Entry{Kind: KindUpstreamNetErr, UpstreamURL: "https://user:password@example.invalid/path-secret?token=query-secret", UpstreamFullURL: "https://user:password@example.invalid/path-secret/v1/chat/completions?token=query-secret"})
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"path-secret", "query-secret", "password", "user"} {
		if strings.Contains(string(data), secret) {
			t.Errorf("error log contains %s", secret)
		}
	}
	var got Entry
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.UpstreamURL != "https://example.invalid" || got.UpstreamFullURL != "https://example.invalid" {
		t.Errorf("unexpected URL fields: %+v", got)
	}
}
