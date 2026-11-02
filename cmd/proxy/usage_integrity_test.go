package main

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/nosql"
)

func TestUsageIntegrityIntervalFromEnvironment(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, tc := range []struct {
		env  string
		want time.Duration
	}{
		{"", nosql.UsageIntegrityClassificationInterval},
		{"60", time.Minute},
		{" 1 ", time.Second},
		{"0", nosql.UsageIntegrityClassificationInterval},
		{"-5", nosql.UsageIntegrityClassificationInterval},
		{"abc", nosql.UsageIntegrityClassificationInterval},
	} {
		t.Setenv("USAGE_INTEGRITY_CLASSIFICATION_INTERVAL_SECONDS", tc.env)
		if got := usageIntegrityIntervalFromEnvironment(logger); got != tc.want {
			t.Errorf("env %q: interval %v, want %v", tc.env, got, tc.want)
		}
	}
	if nosql.UsageIntegrityClassificationInterval != 5*time.Minute {
		t.Fatalf("default interval %v, want 5m", nosql.UsageIntegrityClassificationInterval)
	}
}
