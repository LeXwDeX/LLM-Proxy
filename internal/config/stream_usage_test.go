package config

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestStreamUsageTargetJSON(t *testing.T) {
	for _, raw := range []string{`{}`, `{"request_stream_usage_if_missing":false}`, `{"request_stream_usage_if_missing":true}`} {
		var target Target
		if err := json.Unmarshal([]byte(raw), &target); err != nil {
			t.Fatal(err)
		}
		want := bytes.Contains([]byte(raw), []byte("true"))
		if target.RequestStreamUsageIfMissing != want {
			t.Fatalf("decode %s", raw)
		}
		b, err := json.Marshal(target)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(b, []byte("request_stream_usage_if_missing")) != want {
			t.Fatalf("omitempty: %s", b)
		}
		var restored Target
		if err := json.Unmarshal(b, &restored); err != nil {
			t.Fatal(err)
		}
		if restored.RequestStreamUsageIfMissing != want {
			t.Fatal("round-trip")
		}
	}
}
