package errorlog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

func TestRespExcerptTruncatedTo1024(t *testing.T) {
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
	if len(got.RespExcerpt) != 1024 {
		t.Errorf("excerpt len = %d, want 1024", len(got.RespExcerpt))
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
