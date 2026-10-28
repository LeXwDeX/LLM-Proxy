package errorlog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/natefinch/lumberjack.v2"
)

func TestIssue74ErrorLogRejectsRawDiagnostics(t *testing.T) {
	path := filepath.Join(t.TempDir(), "errors.log")
	Init(path)
	t.Cleanup(func() { _ = Close() })
	Write(Entry{
		TraceID: "synthetic-trace", Kind: KindUpstreamNetErr, UpstreamStatus: 502,
		UpstreamURL:     "http://fixture-user:fixture-password@example.invalid/base?tenant=fixture-query#fixture-fragment",
		UpstreamFullURL: "http://fixture-user:fixture-password@example.invalid/v1/chat?arbitrary=fixture-query",
		RespExcerpt:     "fixture-response-body", Error: "fixture-raw-error",
	})
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"fixture-user", "fixture-password", "fixture-query", "fixture-fragment", "fixture-response-body", "fixture-raw-error"} {
		if strings.Contains(string(data), secret) {
			t.Errorf("error log exposed %q: %s", secret, data)
		}
	}
	var entry Entry
	if err := json.Unmarshal(data, &entry); err != nil {
		t.Fatal(err)
	}
	if entry.TraceID != "synthetic-trace" || entry.Kind != KindUpstreamNetErr || entry.UpstreamStatus != 502 || entry.Error == "" {
		t.Errorf("lost diagnostic metadata: %+v", entry)
	}
	if entry.UpstreamURL != "http://example.invalid" || entry.UpstreamFullURL != "http://example.invalid" {
		t.Errorf("unexpected URL diagnostics: %+v", entry)
	}
}

func TestIssue74LogRotationKeepsExistingFileMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "errors.log")
	Init(path)
	t.Cleanup(func() { _ = Close() })
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	Write(Entry{Kind: KindUpstream4xx, UpstreamStatus: 401})
	rolling, ok := closer.(*lumberjack.Logger)
	if !ok {
		t.Fatal("missing rotating writer")
	}
	if err := rolling.Rotate(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("rotation changed active log mode: %o", info.Mode().Perm())
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		backups, err := filepath.Glob(filepath.Join(filepath.Dir(path), "errors-*.log.gz"))
		if err != nil {
			t.Fatal(err)
		}
		if len(backups) == 1 {
			if _, err := os.Stat(strings.TrimSuffix(backups[0], ".gz")); os.IsNotExist(err) {
				backup, err := os.Stat(backups[0])
				if err != nil || backup.Mode().Perm() != 0o600 {
					t.Fatalf("compressed backup mode: %v %v", backup, err)
				}
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("rotated backup was not compressed")
}
