package logging

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSetupCreatesLogDirectoriesAndFiles(t *testing.T) {
	tempDir := t.TempDir()
	accessPath := filepath.Join(tempDir, "logs", "access.log")
	errorPath := filepath.Join(tempDir, "logs", "error.log")

	manager, err := Setup(Config{
		Level:         "info",
		AccessLogPath: accessPath,
		ErrorLogPath:  errorPath,
	})
	if err != nil {
		t.Fatalf("expected Setup to succeed, got %v", err)
	}
	t.Cleanup(func() { _ = manager.Close() })

	if info, err := os.Stat(filepath.Dir(accessPath)); err != nil {
		t.Fatalf("expected access log directory to exist, got %v", err)
	} else if !info.IsDir() {
		t.Fatalf("expected access log directory to be a directory, got %v", info.Mode())
	}

	if info, err := os.Stat(accessPath); err != nil {
		t.Fatalf("expected access log file to exist, got %v", err)
	} else if info.IsDir() {
		t.Fatalf("expected access log path to be file, got directory")
	}

	if info, err := os.Stat(errorPath); err != nil {
		t.Fatalf("expected error log file to exist, got %v", err)
	} else if info.IsDir() {
		t.Fatalf("expected error log path to be file, got directory")
	}
}

func TestSetupRejectsDirectoryLogPath(t *testing.T) {
	tempDir := t.TempDir()
	_, err := Setup(Config{
		Level:         "info",
		AccessLogPath: tempDir,
		ErrorLogPath:  filepath.Join(tempDir, "error.log"),
	})
	if err == nil {
		t.Fatal("expected Setup to fail when access log path is a directory")
	}
}

func TestParseLevel(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "debug", input: "debug", want: "DEBUG"},
		{name: "warn", input: "warn", want: "WARN"},
		{name: "warning", input: "warning", want: "WARN"},
		{name: "error", input: "error", want: "ERROR"},
		{name: "default", input: "nope", want: "INFO"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseLevel(tc.input).String()
			if got != tc.want {
				t.Fatalf("parseLevel(%q)=%s, want %s", tc.input, got, tc.want)
			}
		})
	}
}
