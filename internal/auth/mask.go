package auth

import "strings"

// MaskClientKey returns the viewer-safe form of a client access key. Client
// keys have no minimum length, so a prefix-plus-suffix mask could reveal most
// of a short key: viewers see at most the last four characters of a key that
// is at least 16 bytes long, and shorter keys are fully hidden.
func MaskClientKey(key string) string {
	if len(key) < 16 {
		return "****"
	}
	return "****" + key[len(key)-4:]
}

// NormalizeClientKeyMask rewrites a stored client-key mask to the viewer-safe
// form. Only the current suffix-only format ("****" + last 4) is preserved;
// that shape is treated as a mask even though a raw 8-byte value could mimic
// it. Legacy audit and trace masks do not encode the original key length, so a
// short key cannot be distinguished from a long one; those values and anything
// unrecognized are fully hidden.
func NormalizeClientKeyMask(mask string) string {
	switch {
	case mask == "":
		return ""
	case len(mask) == 8 && strings.HasPrefix(mask, "****"):
		return mask
	default:
		return "****"
	}
}
