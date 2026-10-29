// runtime.go owns environment overrides, usage retention and HTTP draining.
package main

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/nosql"
	"github.com/LeXwDeX/LLM-Proxy/internal/proxy"
	"github.com/LeXwDeX/LLM-Proxy/internal/requeststats"
	bolt "go.etcd.io/bbolt"
)

// observationDecodeBudgetEnv sizes the shared decode budget (MiB) of
// compressed-response usage observation; empty, 0 or negative = default.
const observationDecodeBudgetEnv = "LLMS_OBSERVATION_DECODE_BUDGET_MB"

// observationDecodeBudgetBytes parses LLMS_OBSERVATION_DECODE_BUDGET_MB and
// returns the budget in bytes, plus a warning when the value was invalid.
func observationDecodeBudgetBytes(raw string) (int64, string) {
	def := int64(proxy.DefaultObservationDecodeBudgetBytes)
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return def, ""
	}
	mb, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || mb > math.MaxInt64>>20 {
		return def, "invalid " + observationDecodeBudgetEnv + ", using default"
	}
	if mb <= 0 {
		return def, ""
	}
	return mb << 20, ""
}

// configureObservationBudgetFromEnvironment installs the decode budget before
// the proxy serves any request.
func configureObservationBudgetFromEnvironment(logger *slog.Logger) {
	raw := os.Getenv(observationDecodeBudgetEnv)
	budget, warning := observationDecodeBudgetBytes(raw)
	if warning != "" {
		logger.Warn(warning, "value", raw)
	}
	budget = proxy.ConfigureObservationDecodeBudget(budget, logger)
	logger.Info("response observation decode budget", "budget_mb", budget>>20)
}

func configureTraceFromEnvironment(trace config.TraceStoreConfig, logger *slog.Logger) config.TraceStoreConfig {
	// Trace store: 仅通过环境变量控制，不从 config.json 读取
	if envTrace := strings.TrimSpace(os.Getenv("TRACE_STORE_ENABLED")); envTrace == "true" {
		trace.Enabled = true
		// 读取可选的磁盘配置
		if envMaxSize := strings.TrimSpace(os.Getenv("TRACE_STORE_MAX_SIZE_MB")); envMaxSize != "" {
			if v, err := strconv.Atoi(envMaxSize); err == nil && v > 0 {
				trace.DiskMaxSizeMB = v
			} else {
				logger.Warn("invalid TRACE_STORE_MAX_SIZE_MB, using default", "value", envMaxSize)
			}
		}
		if envMaxBackups := strings.TrimSpace(os.Getenv("TRACE_STORE_MAX_BACKUPS")); envMaxBackups != "" {
			if v, err := strconv.Atoi(envMaxBackups); err == nil && v > 0 {
				trace.DiskMaxBackups = v
			} else {
				logger.Warn("invalid TRACE_STORE_MAX_BACKUPS, using default", "value", envMaxBackups)
			}
		}
		if envTTL := strings.TrimSpace(os.Getenv("TRACE_STORE_TTL_HOURS")); envTTL != "" {
			if v, err := strconv.Atoi(envTTL); err == nil && v > 0 {
				trace.DiskTTLHours = v
			} else {
				logger.Warn("invalid TRACE_STORE_TTL_HOURS, using default", "value", envTTL)
			}
		}
		logger.Info("trace store enabled via TRACE_STORE_ENABLED=true",
			"disk_max_size_mb", trace.DiskMaxSizeMB,
			"disk_max_backups", trace.DiskMaxBackups,
			"disk_ttl_hours", trace.DiskTTLHours,
		)
	} else {
		trace.Enabled = false
	}
	return trace
}

// 优雅关闭：SIGINT/SIGTERM 时先停止接收新连接并排空在途请求，再由 main 的
// defer 链依次关闭 quota/tracestore/bbolt/日志，避免部署时强杀在途流式请求
// 且 defer 清理全部不执行（bbolt 未 Close、日志缓冲丢失）。
// 宽限期默认 30s，可用 SHUTDOWN_GRACE_SECONDS 覆盖（需同步调大容器 stop 超时）。
func shutdownGraceFromEnvironment(logger *slog.Logger) time.Duration {
	shutdownGrace := 30 * time.Second
	if envGrace := strings.TrimSpace(os.Getenv("SHUTDOWN_GRACE_SECONDS")); envGrace != "" {
		if v, err := strconv.Atoi(envGrace); err == nil && v > 0 {
			shutdownGrace = time.Duration(v) * time.Second
		} else {
			logger.Warn("invalid SHUTDOWN_GRACE_SECONDS, using default", "value", envGrace)
		}
	}
	return shutdownGrace
}

// Issue #125：用量完整性分类巡检间隔，默认 5 分钟，可用
// USAGE_INTEGRITY_CLASSIFICATION_INTERVAL_SECONDS 覆盖（正整数秒）。
func usageIntegrityIntervalFromEnvironment(logger *slog.Logger) time.Duration {
	interval := nosql.UsageIntegrityClassificationInterval
	if env := strings.TrimSpace(os.Getenv("USAGE_INTEGRITY_CLASSIFICATION_INTERVAL_SECONDS")); env != "" {
		if v, err := strconv.Atoi(env); err == nil && v > 0 {
			interval = time.Duration(v) * time.Second
		} else {
			logger.Warn("invalid USAGE_INTEGRITY_CLASSIFICATION_INTERVAL_SECONDS, using default", "value", env)
		}
	}
	return interval
}

// P9：usage_events 保留期自动清理 —— 明细每请求一条且 bbolt 文件不缩容，
// 无清理则 DB 单调膨胀。默认保留 90 天，USAGE_RETENTION_DAYS 覆盖（0 = 关闭，
// 低于月度额度窗口时提升到最低值）。小时聚合格保留（体积小，供长期统计）。
// 启动后先跑一次，此后每 12h 一次。
func startUsageRetention(ctx context.Context, usageStore *nosql.UsageStore, logger *slog.Logger) {
	retentionDays, retentionWarning := usageRetentionDays(os.Getenv("USAGE_RETENTION_DAYS"))
	if retentionWarning != "" {
		logger.Warn(retentionWarning, "value", os.Getenv("USAGE_RETENTION_DAYS"), "retention_days", retentionDays)
	}
	if retentionDays <= 0 {
		return
	}
	go func() {
		ticker := time.NewTicker(12 * time.Hour)
		defer ticker.Stop()
		for {
			cutoff := time.Now().UTC().AddDate(0, 0, -retentionDays)
			if n, err := usageStore.PruneEventsBefore(cutoff, 5000); err != nil {
				logger.Warn("usage retention prune failed", "error", err)
			} else if n > 0 {
				logger.Info("usage retention prune done", "deleted", n, "cutoff", cutoff.Format(time.RFC3339))
			}
			select {
			case <-ticker.C:
			case <-ctx.Done():
				return
			}
		}
	}()
}

// Independent operational counters are retained even when usage retention is disabled.
func startRequestStatsRetention(ctx context.Context, store *nosql.RequestStatsStore, logger *slog.Logger) {
	go func() {
		ticker := time.NewTicker(12 * time.Hour)
		defer ticker.Stop()
		for {
			if ctx.Err() != nil {
				return
			}
			if _, err := store.PruneBefore(time.Now().UTC().Add(-requeststats.Retention), 10000); err != nil {
				logger.Warn("request stats retention prune failed", "error", err)
			}
			select {
			case <-ticker.C:
			case <-ctx.Done():
				return
			}
		}
	}()
}

// startMmapHeadroomMonitor warns (once) when the database file approaches the
// mapping reserved at open, after which growth remaps and can briefly block
// transactions behind in-flight reports. One stat per hour, off the request path.
func startMmapHeadroomMonitor(ctx context.Context, db *bolt.DB, logger *slog.Logger) {
	path := db.Path() // captured now: the handle may be closed while the goroutine winds down
	go func() {
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for {
			nosql.CheckMmapHeadroom(path, logger)
			select {
			case <-ticker.C:
			case <-ctx.Done():
				return
			}
		}
	}()
}

// serveAndDrain serves until ctx ends, then drains HTTP requests and hijacked
// WebSocket turns within the same grace. It returns an error only when the
// server stopped unexpectedly; that path drains the same way first, because
// connections accepted before the listener failed may still be in flight.
func serveAndDrain(ctx context.Context, server *http.Server, shutdownGrace time.Duration, logger *slog.Logger, drainWebsockets func(context.Context) error) error {
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- server.ListenAndServe()
	}()

	logger.Info("http server starting", "addr", server.Addr)
	select {
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server stopped unexpectedly", "error", err)
			drainServer(server, shutdownGrace, logger, drainWebsockets)
			return err
		}
	case <-ctx.Done():
		logger.Info("shutdown signal received, draining in-flight requests", "grace", shutdownGrace.String())
		drainServer(server, shutdownGrace, logger, drainWebsockets)
	}
	return nil
}

// drainServer waits, within one grace, for in-flight handlers and hijacked
// WebSocket turns (which server.Shutdown does not track) so active turns
// finish and record usage.
func drainServer(server *http.Server, shutdownGrace time.Duration, logger *slog.Logger, drainWebsockets func(context.Context) error) {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	websocketsDrained := make(chan error, 1)
	go func() { websocketsDrained <- drainWebsockets(shutdownCtx) }()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Warn("graceful shutdown incomplete, closing remaining connections", "error", err)
		_ = server.Close()
	}
	if err := <-websocketsDrained; err != nil {
		logger.Warn("WebSocket drain incomplete, canceling remaining turns", "error", err)
	}
}

// finishShutdown is the ordered teardown after serveAndDrain, identical on
// the normal and the error path: stop background work, close the proxy
// service (cancels the remaining WebSocket turns and waits for their usage
// recording), then drain persistence (commit the usage and statistics
// queues, replay the journal). The caller closes the database afterwards.
func finishShutdown(logger *slog.Logger, stopBackground func(), closeService func() error, drainPersistence func()) {
	stopBackground()
	if err := closeService(); err != nil {
		logger.Warn("proxy service close failed", "error", err)
	}
	drainPersistence()
}
