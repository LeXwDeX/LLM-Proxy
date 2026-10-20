package proxy

import (
	"bytes"
	"encoding/binary"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/klauspost/compress/zstd"
)

// Faulted br observations must stay incomplete without changing the
// forwarded bytes or encoding header, for both JSON and SSE.
func TestObservationBrotliFaultsPreserveWire(t *testing.T) {
	for _, stream := range []bool{false, true} {
		body := `{"model":"deepseek-chat","choices":[{"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":20}}`
		ct := "application/json"
		if stream {
			ct = "text/event-stream"
			body = "data: " + body + "\n\ndata: [DONE]\n\n"
		}
		valid := observationFixture(t, "br", []byte(body))
		for _, fault := range []struct {
			name string
			wire []byte
		}{
			{"truncated", valid[:len(valid)-3]},
			{"raw", []byte(body)},
			{"trailing_garbage", append(append([]byte(nil), valid...), "garbage"...)},
			{"gzip_mislabeled", observationFixture(t, "gzip", []byte(body))},
		} {
			name := fault.name + "/json"
			if stream {
				name = fault.name + "/sse"
			}
			t.Run(name, func(t *testing.T) {
				up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", ct)
					w.Header().Set("Content-Encoding", "br")
					_, _ = w.Write(fault.wire)
				}))
				defer up.Close()
				svc, err := NewService(&config.Config{Server: config.ServerConfig{RequestTimeoutSeconds: 5}, Targets: []config.Target{{Name: "probe", EndpointType: config.EndpointTypeOpenAI, UsageAdapter: "deepseek", Endpoint: up.URL, APIKey: "fixture"}}}, newTestLogger())
				if err != nil {
					t.Fatal(err)
				}
				rec := &globalAuditRecorder{}
				svc.SetUsageRecorder(rec)
				req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"deepseek-chat"}`))
				req.Header.Set("Accept-Encoding", "gzip, deflate, br")
				req = req.WithContext(auth.WithPrincipal(req.Context(), newAllowAllPrincipal(t, "fixture")))
				rr := httptest.NewRecorder()
				svc.ServeHTTP(rr, req)
				if rr.Code != 200 || !bytes.Equal(rr.Body.Bytes(), fault.wire) || rr.Header().Get("Content-Encoding") != "br" {
					t.Fatalf("faulted br wire changed: status=%d headers=%v", rr.Code, rr.Header())
				}
				if len(rec.events) != 1 || rec.events[0].UsageStatus != "incomplete" || !rec.events[0].CostIncomplete {
					t.Fatalf("faulted br accepted as complete: %+v", rec.events)
				}
			})
		}
	}
}

func singleSegmentZstd(t *testing.T, body []byte) []byte {
	t.Helper()
	encoder, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1), zstd.WithSingleSegment(true))
	if err != nil {
		t.Fatal(err)
	}
	defer encoder.Close()
	wire := encoder.EncodeAll(body, nil)
	var header zstd.Header
	if err := header.Decode(wire); err != nil || !header.SingleSegment || !header.HasFCS || header.FrameContentSize != uint64(len(body)) {
		t.Fatalf("fixture is not a single-segment frame: %+v err=%v", header, err)
	}
	return wire
}

func paddedUsageJSON(size int) []byte {
	prefix := `{"model":"deepseek-chat","choices":[{"message":{"content":"`
	suffix := `"},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":20}}`
	return []byte(prefix + strings.Repeat("x", size-len(prefix)-len(suffix)) + suffix)
}

// Whole-buffer zstd encoders emit one single-segment frame whose window is
// its content size; tens of MiB must still yield exact usage.
func TestObservationZstdSingleSegmentFrames(t *testing.T) {
	for _, tc := range []struct {
		name   string
		size   int
		stream bool
	}{
		{"json/9MiB", 9 << 20, false},
		{"sse/9MiB", 9 << 20, true},
		{"json/32MiB", 32 << 20, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := paddedUsageJSON(tc.size)
			contentType := "application/json"
			if tc.stream {
				contentType = "text/event-stream"
				body = append(append([]byte("data: "), paddedUsageJSON(tc.size-len("data: \n\ndata: [DONE]\n\n"))...), "\n\ndata: [DONE]\n\n"...)
			}
			wire := singleSegmentZstd(t, body)
			u := newUsageStream(contentType, "zstd", usageParser{protocol: config.ProtocolOpenAIChat})
			for len(wire) > 0 {
				n := min(len(wire), 32<<10)
				_, _ = u.Write(wire[:n])
				wire = wire[n:]
			}
			u.Close()
			if u.err != nil || u.tokens.InputTokens != 100 || u.tokens.OutputTokens != 20 || u.tokens.status != usageStatusComplete {
				t.Fatalf("single-segment usage lost: tokens=%+v err=%v", u.tokens, u.err)
			}
		})
	}
}

// The forwarded single-segment frame keeps its bytes while usage is exact.
func TestObservationZstdSingleSegmentService(t *testing.T) {
	wire := singleSegmentZstd(t, paddedUsageJSON(32<<20))
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "zstd")
		_, _ = w.Write(wire)
	}))
	defer up.Close()
	svc, err := NewService(&config.Config{Server: config.ServerConfig{RequestTimeoutSeconds: 30}, Targets: []config.Target{{Name: "probe", EndpointType: config.EndpointTypeOpenAI, UsageAdapter: "standard", Endpoint: up.URL, APIKey: "fixture"}}}, newTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	rec := &globalAuditRecorder{}
	svc.SetUsageRecorder(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"deepseek-chat"}`))
	req.Header.Set("Accept-Encoding", "zstd")
	req = req.WithContext(auth.WithPrincipal(req.Context(), newAllowAllPrincipal(t, "fixture")))
	rr := httptest.NewRecorder()
	svc.ServeHTTP(rr, req)
	if rr.Code != 200 || !bytes.Equal(rr.Body.Bytes(), wire) || rr.Header().Get("Content-Encoding") != "zstd" {
		t.Fatal("forwarded single-segment frame changed")
	}
	if len(rec.events) != 1 || rec.events[0].UsageStatus != "complete" || rec.events[0].InputTokens != 100 || rec.events[0].OutputTokens != 20 {
		t.Fatalf("single-segment service usage lost: %+v", rec.events)
	}
}

// Declared content sizes above the observation bound are rejected from the
// frame header, before the decoder allocates a history buffer of that size.
func TestObservationZstdSingleSegmentOverLimit(t *testing.T) {
	for _, tc := range []struct {
		name string
		size uint64
	}{
		{"just_over", observationZstdSingleSegmentLimit + 1},
		{"one_TiB", 1 << 40},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Single_Segment_Flag with an 8-byte Frame_Content_Size, then one
			// raw last block; the declared size is never actually produced.
			wire := []byte{0x28, 0xb5, 0x2f, 0xfd, 0xe0}
			wire = binary.LittleEndian.AppendUint64(wire, tc.size)
			wire = append(wire, 0x21, 0, 0, 'x', 'y', 'z', 'w')
			var before, after runtime.MemStats
			runtime.GC()
			runtime.ReadMemStats(&before)
			reader, err := observationReader(bytes.NewReader(wire), "zstd")
			if err == nil {
				_, err = io.ReadAll(reader)
				_ = reader.Close()
			}
			runtime.ReadMemStats(&after)
			if err == nil {
				t.Fatal("over-limit single-segment frame accepted")
			}
			if grown := after.TotalAlloc - before.TotalAlloc; grown > 8<<20 {
				t.Fatalf("over-limit frame allocated %d bytes", grown)
			}
			done := make(chan *usageStream)
			go func() {
				u := newUsageStream("application/json", "zstd", usageParser{protocol: config.ProtocolOpenAIChat})
				_, _ = u.Write(wire)
				u.Close()
				done <- u
			}()
			select {
			case u := <-done:
				if u.err == nil || u.tokens.status == usageStatusComplete {
					t.Fatalf("over-limit frame observed as complete: tokens=%+v err=%v", u.tokens, u.err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("over-limit observation blocked forwarding")
			}
		})
	}
}
