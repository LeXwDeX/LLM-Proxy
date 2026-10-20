package main

import (
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/proxy"
)

// LLMS_OBSERVATION_DECODE_BUDGET_MB sizes the shared decode budget of
// compressed-response usage observation in MiB. Empty, 0 or negative selects
// the default (512 MiB); an invalid value warns and uses the default.
func TestObservationDecodeBudgetBytes(t *testing.T) {
	def := int64(proxy.DefaultObservationDecodeBudgetBytes)
	if def != 512<<20 {
		t.Fatalf("default=%d want 512 MiB", def)
	}
	for _, tc := range []struct {
		raw     string
		want    int64
		warning bool
	}{
		{"", def, false},
		{"0", def, false},
		{"-5", def, false},
		{" 1024 ", 1024 << 20, false},
		{"64", 64 << 20, false},
		{"abc", def, true},
		{"1.5", def, true},
		{"9223372036854775807", def, true}, // overflows bytes
	} {
		got, warning := observationDecodeBudgetBytes(tc.raw)
		if got != tc.want || (warning != "") != tc.warning {
			t.Errorf("observationDecodeBudgetBytes(%q) = %d, %q; want %d, warning=%v", tc.raw, got, warning, tc.want, tc.warning)
		}
	}
}
