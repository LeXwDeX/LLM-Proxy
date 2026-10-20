package clientip

import (
	"net/netip"
	"testing"
)

func TestParseAndMatchIPv4Rules(t *testing.T) {
	rules, err := Parse("192.168.0.0/16, 192.168.50.17/24\n10.0.1.1 10.0.1.1")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if rules.Canonical != "192.168.0.0/16,192.168.50.0/24,10.0.1.1" {
		t.Fatalf("canonical = %q", rules.Canonical)
	}
	for _, test := range []struct {
		address string
		allowed bool
	}{
		{"192.168.50.83", true},
		{"192.168.255.255", true},
		{"10.0.1.1", true},
		{"10.0.1.2", false},
		{"2001:db8::1", false},
	} {
		if got := rules.Contains(netip.MustParseAddr(test.address)); got != test.allowed {
			t.Errorf("Contains(%s) = %t, want %t", test.address, got, test.allowed)
		}
	}
}

func TestParseRejectsInvalidRules(t *testing.T) {
	for _, value := range []string{
		"192.168.1.999", "2001:db8::1", "::ffff:192.0.2.1",
		"192.168.0.0/33", "192.168.0.0/-1", "192.168.0.0/abc",
		"192.168.0.0/16,", ",192.168.0.0/16", "192.168.0.0/16,,10.0.1.1",
	} {
		if _, err := Parse(value); err == nil {
			t.Errorf("Parse(%q) should fail", value)
		}
	}
	if rules, err := Parse(" \n "); err != nil || rules.Canonical != "" || len(rules.Prefixes) != 0 {
		t.Fatalf("empty rules = %+v, %v", rules, err)
	}
}
