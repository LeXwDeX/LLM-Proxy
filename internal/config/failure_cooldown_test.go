package config

import (
	"strings"
	"testing"
)

func TestValidateFailureCooldownSeconds(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Server.FailureCooldownSeconds = map[string]int{"429": 10, "403": 3600, "network": 30}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid overrides rejected: %v", err)
	}
	cfg.Server.FailureCooldownSeconds = map[string]int{"418": 10}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), `"418"`) {
		t.Fatalf("unknown class accepted: %v", err)
	}
	cfg.Server.FailureCooldownSeconds = map[string]int{"429": 0}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "must be > 0") {
		t.Fatalf("zero cooldown accepted: %v", err)
	}
}
