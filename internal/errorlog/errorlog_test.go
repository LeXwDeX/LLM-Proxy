package errorlog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestInitWritesNDJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "upstream-error.log")

	Init(path)
	defer Close()

	Write(Entry{
		TraceID:        "trace-abc",
		Kind:           KindUpstream5xx,
		Method:         "POST",
		Path:           "/v1/images/edits",
		Target:         "OpenAI-Image-Edits",
		EndpointType:   "openai_image",
		UpstreamStatus: 502,
		DurationMS:     162790,
		RespExcerpt:    "<html><body>502 Bad Gateway</body></html>",
	})

	// 关闭以确保 lumberjack 落盘。
	if err := Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	line := strings.TrimSpace(string(data))
	if line == "" {
		t.Fatal("file empty")
	}

	var got Entry
	if err := json.Unmarshal([]byte(line), &got); err != nil {
		t.Fatalf("unmarshal: %v line=%q", err, line)
	}
	if got.TraceID != "trace-abc" || got.Kind != KindUpstream5xx || got.UpstreamStatus != 502 || got.DurationMS != 162790 {
		t.Errorf("unexpected entry: %+v", got)
	}
	if got.TS == "" || got.Level != "error" {
		t.Errorf("ts/level not auto-set: %+v", got)
	}
}

func TestRespExcerptDoesNotStoreResponseBody(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "trunc.log")
	Init(path)
	defer Close()

	big := strings.Repeat("x", 2000)
	Write(Entry{TraceID: "t", Kind: KindUpstream4xx, RespExcerpt: big})
	_ = Close()

	data, _ := os.ReadFile(path)
	var got Entry
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(data))), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.RespExcerpt != "[redacted]" {
		t.Errorf("excerpt = %q, want controlled placeholder", got.RespExcerpt)
	}
}

func TestWriteBeforeInitIsNoop(t *testing.T) {
	// 重置全局状态
	_ = Close()

	// 不调用 Init，writer 应为 io.Discard
	// 这次 Write 不应 panic 也不应写到任何地方
	Write(Entry{TraceID: "noop", Kind: KindUpstream5xx})
}

func TestInitWithBadPathDoesNotPanic(t *testing.T) {
	_ = Close()
	// /proc/self/mem 不可写普通内容，但首先目录探测会失败
	Init("/proc/this-must-not-exist/upstream-error.log")
	// 后续 Write 应静默不报错
	Write(Entry{TraceID: "bad", Kind: KindProxyPanic})
	_ = Close()
}

// #186: the controlled error fields are bounded, single-line, valid UTF-8 and
// the parse marker cannot be spoofed by an arbitrary value.
func TestEntryErrorFieldsSanitized(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fields.log")
	Init(path)
	defer Close()

	Write(Entry{
		TraceID:         "t",
		Kind:            KindUpstream4xx,
		RespErrorCode:   "  " + strings.Repeat("汉", 100) + "\n\tcode",
		RespErrorType:   "invalid_request_error\r\n",
		RespErrorParam:  "tools[0].parameters\x00",
		RespErrorStatus: strings.Repeat("s", maxEntryErrorFieldLen+40),
		RespErrorParse:  "anything-else",
		ContentType:     "application/json; charset=utf-8",
	})
	_ = Close()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var got Entry
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(data))), &got); err != nil {
		t.Fatalf("unmarshal: %v line=%q", err, data)
	}
	if len(got.RespErrorCode) > maxEntryErrorFieldLen {
		t.Fatalf("resp_error_code length = %d, want <= %d", len(got.RespErrorCode), maxEntryErrorFieldLen)
	}
	if !utf8.ValidString(got.RespErrorCode) {
		t.Fatalf("resp_error_code is not valid UTF-8: %q", got.RespErrorCode)
	}
	if got.RespErrorCode != strings.Repeat("汉", maxEntryErrorFieldLen/3) {
		t.Fatalf("resp_error_code = %q, want truncated runes", got.RespErrorCode)
	}
	if strings.ContainsAny(got.RespErrorCode+got.RespErrorType+got.RespErrorParam, "\r\n\t\x00") {
		t.Fatalf("control characters survived: %+v", got)
	}
	if got.RespErrorType != "invalid_request_error" || got.RespErrorParam != "tools[0].parameters" {
		t.Fatalf("fields = type %q param %q", got.RespErrorType, got.RespErrorParam)
	}
	if len(got.RespErrorStatus) != maxEntryErrorFieldLen {
		t.Fatalf("resp_error_status length = %d, want %d", len(got.RespErrorStatus), maxEntryErrorFieldLen)
	}
	if got.RespErrorParse != "failed" {
		t.Fatalf("resp_error_parse = %q, want failed", got.RespErrorParse)
	}
	if got.ContentType != "application/json" {
		t.Fatalf("content_type = %q, want media type only", got.ContentType)
	}
}
