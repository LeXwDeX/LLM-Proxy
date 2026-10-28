// Package errorlog 提供上游错误的结构化 NDJSON 日志，与 access/error log 旁路并行。
//
// 触发场景：
//   - 上游 HTTP status >= 400（含 4xx 与 5xx）
//   - 上游网络错误（DNS / connect refused / TLS / read reset）
//   - proxy panic / target 池清空
//
// 不触发：客户端主动 abort、healthz/admin 自身路由错误。
//
// 关联键：trace_id（== middleware.RequestID 注入的 X-Request-ID），与 access log 的 request_id 同值，
// 与下游调用方的 upstream-error.log 的 trace_id 对齐，可双向 grep。
//
// 文件路径：环境变量 UPSTREAM_ERROR_LOG_PATH 覆盖，默认 /var/log/llms-proxy/upstream-error.log。
// 打不开时降级为 noop，不阻断启动。
//
// 轮转：lumberjack 内置（20MB / 30 份 / 30 天 / gzip），与现有 logging 模式一致。
package errorlog

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	"gopkg.in/natefinch/lumberjack.v2"
)

// DefaultPath 为 UPSTREAM_ERROR_LOG_PATH 未设置时的默认路径。
const DefaultPath = "/var/log/llms-proxy/upstream-error.log"

// Kind 标识错误事件类型。与下游调用方约定一致。
type Kind string

const (
	KindUpstream4xx     Kind = "upstream_4xx"
	KindUpstream5xx     Kind = "upstream_5xx"
	KindUpstreamNetErr  Kind = "upstream_net_error"
	KindProxyPanic      Kind = "proxy_panic"
	KindTargetPoolEmpty Kind = "target_pool_empty"
)

// Entry 为一行 NDJSON 错误事件。字段顺序固定（json 包按 struct tag 顺序）。
type Entry struct {
	TS              string `json:"ts"`                          // ISO8601 UTC 毫秒
	Level           string `json:"level"`                       // 固定 "error"
	TraceID         string `json:"trace_id"`                    // 关联 access log request_id
	Kind            Kind   `json:"kind"`                        //
	Method          string `json:"method,omitempty"`            //
	Path            string `json:"path,omitempty"`              // r.URL.Path（不含 query）
	ClientIP        string `json:"client_ip,omitempty"`         //
	Target          string `json:"target,omitempty"`            // config.json 中的 target name
	EndpointType    string `json:"endpoint_type,omitempty"`     //
	UpstreamURL     string `json:"upstream_url,omitempty"`      // 仅 scheme 和 host
	UpstreamFullURL string `json:"upstream_full_url,omitempty"` // 仅 scheme 和 host
	UpstreamStatus  int    `json:"upstream_status,omitempty"`   // net error 时为 0
	DurationMS      int64  `json:"duration_ms,omitempty"`       //
	ReqBytes        int    `json:"req_bytes,omitempty"`         //
	RespBytes       int    `json:"resp_bytes,omitempty"`        //
	RespExcerpt     string `json:"resp_excerpt,omitempty"`      // 有正文时为固定提示，不含正文
	Error           string `json:"error,omitempty"`             // 受控错误类别
}

// ErrorCategory classifies errors without exposing their message or URL.
func ErrorCategory(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.Canceled):
		return "context_canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	case errors.Is(err, io.ErrUnexpectedEOF):
		return "unexpected_eof"
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "dns_error"
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		if netErr.Timeout() {
			return "timeout"
		}
		return "network_error"
	}
	return "error"
}

func safeURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" || u.Opaque != "" {
		return ""
	}
	return (&url.URL{Scheme: u.Scheme, Host: u.Host}).String()
}

// 模块级单例。Init 失败后 logger 仍为 noop，Write 安全可调。
var (
	mu     sync.Mutex
	writer io.Writer = io.Discard
	closer io.Closer
	logger = slog.Default()
)

// SetSlogger 注入应用 slog.Logger，用于内部告警（非业务日志）。
func SetSlogger(l *slog.Logger) {
	if l != nil {
		logger = l
	}
}

// Init 打开错误日志文件并初始化 lumberjack 轮转。失败时降级为 noop（不返回 error），
// 仅通过 slog.Warn 提示运维。这样 errorlog 故障不会阻断 proxy 启动。
func Init(path string) {
	mu.Lock()
	defer mu.Unlock()

	if path == "" {
		path = DefaultPath
	}

	dir := filepath.Dir(path)
	if dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			logger.Warn("upstream-error log disabled (mkdir failed)", "path", path, "error", err)
			return
		}
	}

	// 预探测可写：避免 lumberjack 在第一次 Write 时才报错。
	probe, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		logger.Warn("upstream-error log disabled (open failed)", "path", path, "error", err)
		return
	}
	_ = probe.Close()

	rolling := &lumberjack.Logger{
		Filename:   path,
		MaxSize:    20, // MB
		MaxBackups: 30,
		MaxAge:     30, // days
		Compress:   true,
	}
	writer = rolling
	closer = rolling
	logger.Info("upstream-error log initialized", "path", path)
}

// Close 关闭底层 lumberjack writer。多次调用安全。
func Close() error {
	mu.Lock()
	defer mu.Unlock()
	if closer == nil {
		return nil
	}
	err := closer.Close()
	closer = nil
	writer = io.Discard
	return err
}

// Write 同步追加一行 NDJSON。entry.TS / entry.Level 会被强制覆盖以保证一致性。
// 失败仅 slog.Warn，不返回 error（错误日志失败不应影响请求处理）。
func Write(entry Entry) {
	entry.TS = time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	entry.Level = "error"

	entry.UpstreamURL = safeURL(entry.UpstreamURL)
	entry.UpstreamFullURL = safeURL(entry.UpstreamFullURL)
	if entry.RespExcerpt != "" {
		entry.RespExcerpt = "[redacted]"
	}
	switch entry.Error {
	case "", "error", "context_canceled", "deadline_exceeded", "unexpected_eof", "dns_error", "timeout", "network_error", "panic":
	default:
		entry.Error = "error"
	}

	line, err := json.Marshal(entry)
	if err != nil {
		logger.Warn("upstream-error log marshal failed", "error", err, "trace_id", entry.TraceID)
		return
	}

	mu.Lock()
	w := writer
	mu.Unlock()
	if w == io.Discard {
		return
	}

	line = append(line, '\n')
	if _, err := w.Write(line); err != nil {
		logger.Warn("upstream-error log write failed", "error", err, "trace_id", entry.TraceID)
	}
}
