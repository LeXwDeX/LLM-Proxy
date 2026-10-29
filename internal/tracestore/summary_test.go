package tracestore

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// Issue #97: the trace list ships a projection without bodies or headers; the
// detail endpoint keeps the full record.
func TestSummaryDropsBodiesAndHeaders(t *testing.T) {
	rec := &TraceRecord{
		TraceID: "t1", Timestamp: time.Unix(1700000000, 0).UTC(), ClientName: "c", Target: "tg", Model: "m",
		StatusCode: 200, DurationMS: 12, InputTokens: 3, OutputTokens: 4,
		RequestHeaders:  map[string]string{"Authorization": "secret"},
		UpstreamHeaders: map[string]string{"X": "y"},
		RequestBody:     strings.Repeat("请", 10), ResponseBody: "ok",
	}
	data, err := json.Marshal(rec.Summary())
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	for _, banned := range []string{"request_body", "response_body", "request_headers", "upstream_headers", "client_access_key", "client_ip"} {
		if _, ok := got[banned]; ok {
			t.Errorf("summary must not carry %s", banned)
		}
	}
	for key, want := range map[string]any{"trace_id": "t1", "client_name": "c", "target": "tg", "model": "m", "status_code": 200.0, "duration_ms": 12.0, "input_tokens": 3.0, "output_tokens": 4.0, "request_body_bytes": 30.0, "response_body_bytes": 2.0} {
		if got[key] != want {
			t.Errorf("%s = %v, want %v", key, got[key], want)
		}
	}
}
