package config

import "testing"

func TestAllProtocolTypes(t *testing.T) {
	all := AllProtocolTypes()
	if len(all) != 5 {
		t.Fatalf("expected 5 protocol types, got %d: %v", len(all), all)
	}

	// Verify it returns a copy, not the original slice.
	all[0] = "mutated"
	original := AllProtocolTypes()
	if original[0] == "mutated" {
		t.Fatal("AllProtocolTypes should return a copy, not the original slice")
	}
}

func TestIsValidProtocolType(t *testing.T) {
	validCases := []ProtocolType{
		ProtocolOpenAIChat,
		ProtocolOpenAIResponses,
		ProtocolAnthropicMessages,
		ProtocolGemini,
		ProtocolOpenAIImage,
	}
	for _, p := range validCases {
		if !IsValidProtocolType(p) {
			t.Errorf("expected %q to be valid", p)
		}
	}

	invalidCases := []ProtocolType{
		"",
		"unknown",
		"openai",          // not a protocol type (it's an endpoint_type)
		"OpenAIChat",      // wrong case
		"openai_chat_v2",  // doesn't exist
	}
	for _, p := range invalidCases {
		if IsValidProtocolType(p) {
			t.Errorf("expected %q to be invalid", p)
		}
	}
}
