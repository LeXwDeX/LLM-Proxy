package quota

import (
	"encoding/json"
	"testing"
	"time"
)

// TestExceededInfoJSON 验证 JSON 字段名符合 §5.2。
func TestExceededInfoJSON(t *testing.T) {
	info := ExceededInfo{
		Dimension: DimensionDaily,
		Limit:     10,
		Used:      10.5,
		ResetsAt:  time.Date(2026, 6, 9, 0, 0, 0, 0, time.UTC),
	}
	data, err := json.Marshal(info)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"dimension", "limit", "used", "resets_at"} {
		if _, ok := decoded[key]; !ok {
			t.Errorf("missing JSON field: %s", key)
		}
	}
}

// TestQuotaStatusJSON 验证 §11.2 字段。
func TestQuotaStatusJSON(t *testing.T) {
	status := QuotaStatus{
		Client:   "alice",
		Quotas:   map[string]QuotaUsage{"daily": {Limit: 10, Used: 3, ResetsAt: time.Now().UTC()}},
		Exceeded: nil,
	}
	data, err := json.Marshal(status)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"client", "quotas", "exceeded"} {
		if _, ok := decoded[key]; !ok {
			t.Errorf("missing JSON field: %s", key)
		}
	}
}
