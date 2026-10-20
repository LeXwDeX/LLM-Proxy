package fakellm

import (
	"bufio"
	"compress/gzip"
	"compress/zlib"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/klauspost/compress/zstd"
)

func encodedReader(encoding string, r io.Reader) (io.ReadCloser, error) {
	switch encoding {
	case "gzip":
		return gzip.NewReader(r)
	case "deflate":
		return zlib.NewReader(r)
	case "zstd":
		d, e := zstd.NewReader(r)
		if e != nil {
			return nil, e
		}
		return zstdEncodingReader{d}, nil
	default:
		return io.NopCloser(r), nil
	}
}

func TestResponseEncodingJSONSSEAndLegacy(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		for _, encoding := range []string{"identity", "gzip", "deflate", "zstd"} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("legacy=%v/%s/stream=%v", legacy, encoding, stream), func(t *testing.T) {
					ts := httptest.NewServer(New(Options{Name: "encoding", LegacyMock: legacy}))
					defer ts.Close()
					body := fmt.Sprintf(`{"model":"m","stream":%v,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"[[fake encoding=%s usage.in=100 usage.out=20 usage.cached=30]]"}]}`, stream, encoding)
					resp, wire := post(t, ts.URL+"/openai/v1/chat/completions", body, map[string]string{"Accept-Encoding": "identity"})
					if resp.StatusCode != 200 || resp.Header.Get("Content-Encoding") != encoding {
						t.Fatalf("status=%d encoding=%q body=%s", resp.StatusCode, resp.Header.Get("Content-Encoding"), wire)
					}
					reader, err := encodedReader(encoding, strings.NewReader(string(wire)))
					if err != nil {
						t.Fatal(err)
					}
					defer reader.Close()
					plain, err := io.ReadAll(reader)
					if err != nil {
						t.Fatal(err)
					}
					if stream {
						if !strings.Contains(string(plain), `"prompt_tokens":100`) || !strings.Contains(string(plain), "[DONE]") {
							t.Fatalf("stream=%s", plain)
						}
					} else {
						if got := dig(decode(t, plain), "usage/prompt_tokens"); got != float64(100) {
							t.Fatalf("usage=%v", got)
						}
					}
				})
			}
		}
	}
}

func TestResponseEncodingFlushBeforeStreamCompletion(t *testing.T) {
	for _, encoding := range []string{"gzip", "deflate", "zstd"} {
		t.Run(encoding, func(t *testing.T) {
			fake, ts := newTestServer(t)
			req, _ := http.NewRequest("POST", ts.URL+"/openai/v1/chat/completions", strings.NewReader(fmt.Sprintf(`{"stream":true,"messages":[{"role":"user","content":"[[fake encoding=%s chunks=2 chunk_delay_ms=1000]]"}]}`, encoding)))
			req.Header.Set("Accept-Encoding", "identity")
			start := time.Now()
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			reader, err := encodedReader(encoding, resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			line, err := bufio.NewReader(reader).ReadString('\n')
			if err != nil || !strings.HasPrefix(line, "data:") {
				t.Fatalf("first event %q: %v", line, err)
			}
			if time.Since(start) > 700*time.Millisecond {
				t.Fatal("first decompressed event waited for stream completion")
			}
			if len(fake.Requests()) != 0 {
				t.Fatal("stream completed before first event was read")
			}
		})
	}
}

func TestResponseEncodingDoesNotNegotiateByDefault(t *testing.T) {
	_, ts := newTestServer(t)
	resp, body := post(t, ts.URL+"/openai/v1/chat/completions", `{"messages":[]}`, map[string]string{"Accept-Encoding": "gzip, deflate, zstd"})
	if resp.Header.Get("Content-Encoding") != "" {
		t.Fatalf("encoding=%q", resp.Header.Get("Content-Encoding"))
	}
	decode(t, body)
}

func TestResponseEncodingControlRemainsUncompressed(t *testing.T) {
	_, ts := newTestServer(t)
	resp, body := post(t, ts.URL+"/_control", `{"scenario":{"response_encoding":"gzip"}}`, map[string]string{"Accept-Encoding": "identity"})
	if resp.Header.Get("Content-Encoding") != "" {
		t.Fatal("control was compressed")
	}
	var payload any
	if json.Unmarshal(body, &payload) != nil {
		t.Fatal("control body changed")
	}
	resp, body = post(t, ts.URL+"/openai/v1/chat/completions", `{"messages":[]}`, map[string]string{"Accept-Encoding": "identity"})
	if resp.Header.Get("Content-Encoding") != "gzip" {
		t.Fatal("control scenario did not enable model compression")
	}
	r, err := gzip.NewReader(strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	plain, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	decode(t, plain)
}

func TestResponseEncodingFaultsAndApplicationErrors(t *testing.T) {
	for _, encoding := range []string{"gzip", "deflate", "zstd"} {
		for _, mode := range []string{"truncated", "corrupt", "cut", "stream_error"} {
			t.Run(encoding+"/"+mode, func(t *testing.T) {
				_, ts := newTestServer(t)
				fault := "encoding_fault=" + mode
				if mode == "cut" {
					fault = "cut_after=1"
				}
				if mode == "stream_error" {
					fault = "stream_error=1"
				}
				req, _ := http.NewRequest("POST", ts.URL+"/openai/v1/chat/completions", strings.NewReader(fmt.Sprintf(`{"stream":true,"messages":[{"role":"user","content":"[[fake encoding=%s %s]]"}]}`, encoding, fault)))
				req.Header.Set("Accept-Encoding", "identity")
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer resp.Body.Close()
				wire, transportErr := io.ReadAll(resp.Body)
				if mode == "corrupt" && !strings.HasPrefix(string(wire), "invalid-compression-prefix") {
					t.Fatal("corrupt format prefix was not first on wire")
				}
				r, err := encodedReader(encoding, strings.NewReader(string(wire)))
				if err != nil {
					if mode != "corrupt" {
						t.Fatal(err)
					}
					return
				}
				defer r.Close()
				plain, decodeErr := io.ReadAll(r)
				if mode == "stream_error" {
					if decodeErr != nil || transportErr != nil || !strings.Contains(string(plain), `"error"`) {
						t.Fatalf("application error broke frame: %v/%v %s", decodeErr, transportErr, plain)
					}
				} else if decodeErr == nil {
					t.Fatalf("%s unexpectedly has valid compressed trailer", mode)
				}
			})
		}
	}
}

func TestResponseEncodingHeaderDeclarations(t *testing.T) {
	for _, headers := range [][]string{nil, {}, {" GZiP \t"}, {"identity", "gzip"}, {"gzip, identity"}, {"br"}} {
		t.Run(fmt.Sprintf("%q", headers), func(t *testing.T) {
			fake := New(Options{})
			fake.global = &Scenario{ResponseEncoding: "gzip", ResponseEncodingHeaders: headers}
			rr := httptest.NewRecorder()
			fake.ServeHTTP(rr, httptest.NewRequest("POST", "/openai/v1/chat/completions", strings.NewReader(`{"messages":[]}`)))
			want := headers
			if headers == nil {
				want = []string{"gzip"}
			}
			got := rr.Header().Values("Content-Encoding")
			if strings.Join(got, "|") != strings.Join(want, "|") || len(got) != len(want) {
				t.Fatalf("headers=%q want=%q", got, want)
			}
			r, err := gzip.NewReader(strings.NewReader(rr.Body.String()))
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			plain, err := io.ReadAll(r)
			if err != nil {
				t.Fatal(err)
			}
			decode(t, plain)
		})
	}
	_, ts := newTestServer(t)
	resp, _ := post(t, ts.URL+"/_control", `{"scenario":{"response_encoding":"gzip","response_encoding_headers":["gzip\r\nInjected: yes"]}}`, nil)
	if resp.StatusCode != 400 {
		t.Fatal("unsafe header accepted")
	}
}

func TestResponseEncodingCanceledStreamDoesNotFinishFrame(t *testing.T) {
	for _, encoding := range []string{"gzip", "deflate", "zstd"} {
		t.Run(encoding, func(t *testing.T) {
			fake := New(Options{})
			ctx, cancel := context.WithCancel(context.Background())
			writer := &cancelEncodingWriter{ResponseRecorder: httptest.NewRecorder(), cancel: cancel}
			req := httptest.NewRequest("POST", "/openai/v1/chat/completions", strings.NewReader(fmt.Sprintf(`{"stream":true,"messages":[{"role":"user","content":"[[fake encoding=%s chunks=2 chunk_delay_ms=1]]"}]}`, encoding))).WithContext(ctx)
			func() {
				defer func() {
					if p := recover(); p != http.ErrAbortHandler {
						t.Fatalf("panic=%v", p)
					}
				}()
				fake.ServeHTTP(writer, req)
			}()
			r, err := encodedReader(encoding, strings.NewReader(writer.Body.String()))
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			if _, err := io.ReadAll(r); err == nil {
				t.Fatal("cancel supplied complete encoded trailer")
			}
		})
	}
}

type cancelEncodingWriter struct {
	*httptest.ResponseRecorder
	cancel context.CancelFunc
}

func (w *cancelEncodingWriter) Flush() { w.ResponseRecorder.Flush(); w.cancel() }

func TestResponseEncodingWebsocketUpgradeUnaffected(t *testing.T) {
	fake, ts := newTestServer(t)
	fake.global = &Scenario{ResponseEncoding: "gzip"}
	conn, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(ts.URL, "http")+"/openai/v1/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if resp.Header.Get("Content-Encoding") != "" {
		t.Fatal("compressed WebSocket upgrade")
	}
	if err := conn.WriteJSON(map[string]any{"type": "response.create", "model": "m", "input": []any{map[string]any{"role": "user", "content": "hi"}}}); err != nil {
		t.Fatal(err)
	}
	for {
		var event map[string]any
		if err := conn.ReadJSON(&event); err != nil {
			t.Fatal(err)
		}
		if event["type"] == "response.completed" {
			break
		}
	}
}

type zstdEncodingReader struct{ *zstd.Decoder }

func (r zstdEncodingReader) Close() error { r.Decoder.Close(); return nil }

func TestResponseEncodingWriteFailureDoesNotSupplyTrailer(t *testing.T) {
	for _, encoding := range []string{"gzip", "deflate", "zstd"} {
		t.Run(encoding, func(t *testing.T) {
			fake := New(Options{})
			writer := &failedEncodingWriter{ResponseRecorder: httptest.NewRecorder()}
			req := httptest.NewRequest("POST", "/openai/v1/chat/completions", strings.NewReader(fmt.Sprintf(`{"messages":[{"role":"user","content":"[[fake encoding=%s]]"}]}`, encoding)))
			fake.ServeHTTP(writer, req)
			if writer.writes != 1 {
				t.Fatalf("encoder tried to repair failed wire: writes=%d", writer.writes)
			}
		})
	}
}

type failedEncodingWriter struct {
	*httptest.ResponseRecorder
	writes int
}

func (w *failedEncodingWriter) Write(p []byte) (int, error) { w.writes++; return 0, io.ErrClosedPipe }

func TestResponseEncodingDropsPlaintextContentLength(t *testing.T) {
	fake := New(Options{})
	rr := httptest.NewRecorder()
	rr.Header().Set("Content-Length", "10")
	fake.ServeHTTP(rr, httptest.NewRequest("POST", "/openai/v1/chat/completions", strings.NewReader(`{"messages":[{"role":"user","content":"[[fake encoding=gzip]]"}]}`)))
	if rr.Header().Get("Content-Length") != "" {
		t.Fatal("plaintext Content-Length survived encoding")
	}
	r, err := gzip.NewReader(strings.NewReader(rr.Body.String()))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	body, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	decode(t, body)
}
