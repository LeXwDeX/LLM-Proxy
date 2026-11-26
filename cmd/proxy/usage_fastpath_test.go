package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/nosql"
	bolt "go.etcd.io/bbolt"
)

func fastPathDB(t *testing.T) *bolt.DB {
	t.Helper()
	db, err := nosql.OpenDB(t.TempDir() + "/fast.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { nosql.ForgetUsageIntegrityClassification(db); _ = db.Close() })
	return db
}

// 启动顺序（Issue #250/#252）：先等维护写（索引迁移、首次保留期清理）结束，二者与分类互相作废；
// 之后才启动分类循环，并只观察就绪，不取消构建。
func TestPrepareUsageFastPathStartsClassificationAfterMaintenance(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	db := fastPathDB(t)
	maintenanceDone := make(chan struct{})
	var startedBeforeDone atomic.Bool
	var waited atomic.Int64
	start := func() {
		select {
		case <-maintenanceDone:
		default:
			startedBeforeDone.Store(true)
		}
	}
	waitReady := func(_ context.Context, _ *bolt.DB, timeout time.Duration) bool {
		waited.Store(int64(timeout))
		return true
	}
	go func() { time.Sleep(60 * time.Millisecond); close(maintenanceDone) }()

	ready := prepareUsageFastPath(context.Background(), db, maintenanceDone, logger, 7*time.Second, start, waitReady)
	if startedBeforeDone.Load() {
		t.Fatal("classification started while a maintenance write was still running")
	}
	// One budget: the classification gets what the maintenance wait left over.
	if got := time.Duration(waited.Load()); !ready || got <= 0 || got > 7*time.Second-50*time.Millisecond {
		t.Fatalf("ready=%v waited=%v; want the remainder of the 7s budget after the ~60ms maintenance wait", ready, got)
	}
}

// 维护写迟迟不结束（首次升级的大库）：到上限后仍要启动分类循环（它会在维护写结束后自动建），
// 但不再等待就绪，监听不被拖住。
func TestPrepareUsageFastPathDegradesWhenMaintenanceStalls(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	db := fastPathDB(t)
	var started, waited atomic.Bool
	begin := time.Now()
	ready := prepareUsageFastPath(context.Background(), db, make(chan struct{}), logger, 80*time.Millisecond,
		func() { started.Store(true) },
		func(context.Context, *bolt.DB, time.Duration) bool { waited.Store(true); return true })
	if d := time.Since(begin); d > 3*time.Second {
		t.Fatalf("startup blocked %v on stalled maintenance", d)
	}
	if !started.Load() {
		t.Fatal("the classification loop must still start; it rebuilds once the maintenance write ended")
	}
	if waited.Load() || ready {
		t.Fatalf("must not wait for readiness while maintenance runs (waited=%v ready=%v)", waited.Load(), ready)
	}
}

// 首轮保留期清理很慢（长时间停机后过期明细积压）：它吃掉的是同一份预算，不是额外时间；
// 预算耗尽前清理结束则分类只拿到剩余时间，耗尽仍未结束则降级且不再等待就绪（Issue #252 评审第 1 条）。
func TestPrepareUsageFastPathSlowRetentionConsumesTheSharedBudget(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	db := fastPathDB(t)
	slow := make(chan struct{})
	var waited atomic.Int64
	go func() { time.Sleep(300 * time.Millisecond); close(slow) }()
	begin := time.Now()
	prepareUsageFastPath(context.Background(), db, slow, logger, time.Second, func() {},
		func(_ context.Context, _ *bolt.DB, d time.Duration) bool { waited.Store(int64(d)); return false })
	if d := time.Since(begin); d > 2*time.Second {
		t.Fatalf("startup took %v with a 1s budget", d)
	}
	if got := time.Duration(waited.Load()); got <= 0 || got > 750*time.Millisecond {
		t.Fatalf("classification wait = %v, want the ~700ms left after a 300ms retention round", got)
	}

	// Retention outlasts the budget: degrade, loop started, nothing awaited.
	var started, waitedAgain atomic.Bool
	begin = time.Now()
	ready := prepareUsageFastPath(context.Background(), db, make(chan struct{}), logger, 100*time.Millisecond,
		func() { started.Store(true) },
		func(context.Context, *bolt.DB, time.Duration) bool { waitedAgain.Store(true); return true })
	if d := time.Since(begin); d > time.Second || ready || !started.Load() || waitedAgain.Load() {
		t.Fatalf("overrun: took %v ready=%v started=%v waited=%v", d, ready, started.Load(), waitedAgain.Load())
	}
}

// wait=0：只启动循环，不等待（即使维护写未结束）。
func TestPrepareUsageFastPathZeroWaitDoesNotWait(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	db := fastPathDB(t)
	done := make(chan struct{})
	close(done)
	var started, waited atomic.Bool
	ready := prepareUsageFastPath(context.Background(), db, done, logger, 0,
		func() { started.Store(true) },
		func(context.Context, *bolt.DB, time.Duration) bool { waited.Store(true); return true })
	if !started.Load() || waited.Load() || ready {
		t.Fatalf("started=%v waited=%v ready=%v", started.Load(), waited.Load(), ready)
	}
}

// 停止信号在启动等待期间到来时立即返回，且不启动分类循环。
func TestPrepareUsageFastPathStopsOnCancel(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	db := fastPathDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var started atomic.Bool
	begin := time.Now()
	prepareUsageFastPath(ctx, db, make(chan struct{}), logger, time.Minute,
		func() { started.Store(true) },
		func(context.Context, *bolt.DB, time.Duration) bool { return false })
	if d := time.Since(begin); d > time.Second {
		t.Fatalf("canceled startup waited %v", d)
	}
	if started.Load() {
		t.Fatal("classification started after cancellation")
	}
}

// 等待超时只放弃等待，不取消构建：循环继续跑，最终装好分类（Issue #252：此前超时会丢弃全部进度）。
func TestPrepareUsageFastPathTimeoutKeepsBuilding(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	db := fastPathDB(t)
	done := make(chan struct{})
	close(done)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stop func()
	start := func() {
		stop = nosql.StartUsageIntegrityClassificationWith(ctx, db, nosql.ClassificationLoopOptions{Interval: time.Hour}, logger)
	}
	prepareUsageFastPath(ctx, db, done, logger, time.Nanosecond, start, nosql.WaitUsageIntegrityReady)
	defer stop()
	if !nosql.WaitUsageIntegrityReady(ctx, db, 10*time.Second) {
		t.Fatal("the build was abandoned after the startup wait elapsed")
	}
}

func TestUsageIntegrityStartupWaitFromEnvironment(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, tc := range []struct {
		raw  string
		want time.Duration
	}{
		{"", 20 * time.Second},
		{"0", 0},
		{"45", 45 * time.Second},
		{" 90 ", 90 * time.Second},
		{"301", 300 * time.Second},
		{"10000000000", 300 * time.Second},
		{"99999999999999999999", 300 * time.Second}, // 超出 int64：按上限，不是回退默认
		{"-99999999999999999999", 20 * time.Second},
		{"-1", 20 * time.Second},
		{"abc", 20 * time.Second},
	} {
		t.Setenv("USAGE_INTEGRITY_STARTUP_WAIT_SECONDS", tc.raw)
		if got := usageIntegrityStartupWaitFromEnvironment(logger); got != tc.want {
			t.Errorf("USAGE_INTEGRITY_STARTUP_WAIT_SECONDS=%q: got %v, want %v", tc.raw, got, tc.want)
		}
	}
}

// 首次保留期清理前移到分类之前：返回的通道在首轮清理结束后关闭；关闭保留期时立即关闭。
func TestStartUsageRetentionSignalsFirstRound(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	db := fastPathDB(t)
	store := nosql.NewUsageStore(db)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for _, days := range []string{"0", "90"} {
		t.Setenv("USAGE_RETENTION_DAYS", days)
		done := startUsageRetention(ctx, store, logger)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("USAGE_RETENTION_DAYS=%s: first round never signaled", days)
		}
	}
}

// syncBuf 是可被日志 goroutine 并发写入的缓冲区。
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// Issue #254：维护写（迁移持续失败会无限退避重试）的等待单独封顶为预算的一半，
// 不能每次启动都吃满整份预算。
func TestPrepareUsageFastPathStalledMaintenanceIsCappedAtHalfTheBudget(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	db := fastPathDB(t)
	var started, waited atomic.Bool
	begin := time.Now()
	ready := prepareUsageFastPath(context.Background(), db, make(chan struct{}), logger, 4*time.Second,
		func() { started.Store(true) },
		func(context.Context, *bolt.DB, time.Duration) bool { waited.Store(true); return true })
	d := time.Since(begin)
	if d >= 3*time.Second {
		t.Fatalf("stalled maintenance held startup for %v of a 4s budget; the cap is half of it", d)
	}
	if d < 1800*time.Millisecond {
		t.Fatalf("returned after %v, before the half-budget cap of 2s", d)
	}
	if !started.Load() || waited.Load() || ready {
		t.Fatalf("started=%v waited=%v ready=%v", started.Load(), waited.Load(), ready)
	}
}

// Issue #254：wait=0 是配置的“不等待”，不应报“维护写仍在运行”告警，但仍立即启动循环。
func TestPrepareUsageFastPathZeroWaitDoesNotWarn(t *testing.T) {
	var logs syncBuf
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	db := fastPathDB(t)
	var started atomic.Bool
	prepareUsageFastPath(context.Background(), db, make(chan struct{}), logger, 0,
		func() { started.Store(true) },
		func(context.Context, *bolt.DB, time.Duration) bool { return true })
	if !started.Load() {
		t.Fatal("the classification loop must start immediately even with wait=0")
	}
	if strings.Contains(logs.String(), "maintenance writes still running") {
		t.Fatalf("wait=0 means do not wait; no stalled-maintenance warning expected:\n%s", logs.String())
	}
}
