package usage

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ErrExplicitRecordMissingTimestamp rejects an explicit stable identity that
// has no stable timestamp. Callers must validate before filling defaults.
var ErrExplicitRecordMissingTimestamp = errors.New("usage: explicit record ID requires a non-zero timestamp")

// ValidateExplicitRecordIdentity rejects an explicit RecordID without a
// timestamp. An empty RecordID is an ordinary new event and keeps automatic
// identity generation.
func ValidateExplicitRecordIdentity(event Event) error {
	if strings.TrimSpace(event.RecordID) == "" || !event.Timestamp.IsZero() {
		return nil
	}
	return fmt.Errorf("%w: %q", ErrExplicitRecordMissingTimestamp, strings.TrimSpace(event.RecordID))
}

// RetryEquivalentDigest normalizes an event for retry identity comparison.
// PricingRuleID and PricingRuleVersion are derived from the currently editable
// price template, so a version-only edit must not look like different usage.
// Every other field, including PricingContext, diagnostics and cost
// completeness, remains part of the digest.
func RetryEquivalentDigest(event Event) (string, error) {
	event = CloneEvent(event)
	event.Timestamp = event.Timestamp.UTC()
	event.PricingRuleID = ""
	event.PricingRuleVersion = 0
	for i := range event.Components {
		event.Components[i].PricingRuleID = ""
		event.Components[i].PricingRuleVersion = 0
	}
	data, err := json.Marshal(event)
	if err != nil {
		return "", fmt.Errorf("encode usage retry identity: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}
