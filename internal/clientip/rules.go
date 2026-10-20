package clientip

import (
	"fmt"
	"net/netip"
	"strings"
)

// Rules is a canonical IPv4 allowlist. An empty list imposes no restriction.
type Rules struct {
	Prefixes  []netip.Prefix
	Canonical string
}

// Parse accepts comma or whitespace separated IPv4 addresses and CIDR ranges.
// Commas must separate nonempty entries; a bare address is treated as a /32.
func Parse(raw string) (Rules, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Rules{}, nil
	}

	var rules Rules
	canonical := make([]string, 0)
	seen := make(map[string]struct{})
	for _, group := range strings.Split(raw, ",") {
		entries := strings.Fields(group)
		if len(entries) == 0 {
			return Rules{}, fmt.Errorf("ip_address contains an empty entry")
		}
		for _, entry := range entries {
			var prefix netip.Prefix
			var display string
			if strings.Contains(entry, "/") {
				parsed, err := netip.ParsePrefix(entry)
				if err != nil || !parsed.Addr().Is4() {
					return Rules{}, fmt.Errorf("ip_address entry %q must be an IPv4 address or CIDR", entry)
				}
				prefix = parsed.Masked()
				display = prefix.String()
			} else {
				address, err := netip.ParseAddr(entry)
				if err != nil || !address.Is4() {
					return Rules{}, fmt.Errorf("ip_address entry %q must be an IPv4 address or CIDR", entry)
				}
				prefix = netip.PrefixFrom(address, 32)
				display = address.String()
			}
			if _, exists := seen[prefix.String()]; exists {
				continue
			}
			seen[prefix.String()] = struct{}{}
			rules.Prefixes = append(rules.Prefixes, prefix)
			canonical = append(canonical, display)
		}
	}
	rules.Canonical = strings.Join(canonical, ",")
	return rules, nil
}

// Contains reports whether an address matches at least one configured entry.
func (r Rules) Contains(address netip.Addr) bool {
	if !address.Is4() {
		return false
	}
	for _, prefix := range r.Prefixes {
		if prefix.Contains(address) {
			return true
		}
	}
	return false
}
