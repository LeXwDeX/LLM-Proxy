package auth

import "testing"

func TestMaskClientKey(t *testing.T) {
	for _, tc := range []struct {
		key  string
		want string
	}{
		{"", "****"},
		{"sk-short", "****"},
		{"123456789012345", "****"},      // 15 bytes: fully hidden
		{"1234567890123456", "****3456"}, // 16 bytes: suffix only
		{"sk-live-client-secret-1234567890", "****7890"},
	} {
		if got := MaskClientKey(tc.key); got != tc.want {
			t.Errorf("MaskClientKey(%q) = %q, want %q", tc.key, got, tc.want)
		}
	}
}

func TestNormalizeClientKeyMask(t *testing.T) {
	for _, tc := range []struct {
		mask string
		want string
	}{
		{"", ""},
		{"****", "****"},
		{"****7890", "****7890"},
		// Legacy audit maskKey (first4 + "..." + last4): the original key
		// length is unknowable, so a possibly short key must stay hidden.
		{"abcd...wxyz", "****"},
		// Legacy trace maskAPIKey (first6 + "***" + last4).
		{"abcdef***wxyz", "****"},
		{"***", "****"},
		// Raw-looking values are never trusted back to the caller.
		{"1234567890123456", "****"},
		{"abcdef***wxyz1", "****"},
		// A value indistinguishable from the current suffix-only format may
		// keep its suffix; this is the documented format assumption.
		{"****abcd", "****abcd"},
	} {
		if got := NormalizeClientKeyMask(tc.mask); got != tc.want {
			t.Errorf("NormalizeClientKeyMask(%q) = %q, want %q", tc.mask, got, tc.want)
		}
	}
}
