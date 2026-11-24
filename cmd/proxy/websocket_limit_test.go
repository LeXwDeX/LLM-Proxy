package main

import (
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/proxy"
)

// LLMS_WS_MAX_MESSAGE_MB sets the native Responses WebSocket single-message
// limit in MiB. Empty, 0 or negative selects the default (29 MiB, just under
// the upstream's 30MB); an invalid value warns and uses the default; a value
// above the request body cap is clamped to it with a warning (#238).
func TestWebsocketMaxMessageBytes(t *testing.T) {
	def := proxy.DefaultWebsocketMaxMessageBytes
	if def != 29<<20 {
		t.Fatalf("default=%d want 29 MiB", def)
	}
	maxBytes := proxy.MaxWebsocketMessageBytes
	for _, tc := range []struct {
		raw     string
		want    int
		warning bool
	}{
		{"", def, false},
		{"0", def, false},
		{"-5", def, false},
		{" 8 ", 8 << 20, false},
		{"30", 30 << 20, false},
		{"100", maxBytes, false},
		{"abc", def, true},
		{"1.5", def, true},
		{"1000", maxBytes, true},
		{"9223372036854775807", maxBytes, true},
	} {
		got, warning := websocketMaxMessageBytes(tc.raw)
		if got != tc.want || (warning != "") != tc.warning {
			t.Errorf("websocketMaxMessageBytes(%q) = %d, %q; want %d, warning=%v", tc.raw, got, warning, tc.want, tc.warning)
		}
	}
}
