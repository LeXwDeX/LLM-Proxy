package main

import (
	"context"
	"io"
	"log/slog"
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

// 重启后 503 窗口的根因之一：额度预加载先于分类安装，校准读全程退回穷举扫描。
// 启动必须先等索引迁移结束（它与分类互相作废），再同步建好分类，最后才轮到额度预加载。
func TestPrepareUsageFastPathOrdersMigrationBeforeClassification(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	db := fastPathDB(t)
	indexDone := make(chan struct{})
	var classifiedBeforeIndexDone atomic.Bool
	classify := func(ctx context.Context, db *bolt.DB) error {
		select {
		case <-indexDone:
		default:
			classifiedBeforeIndexDone.Store(true)
		}
		return nosql.RunClassificationOnce(ctx, db)
	}
	go func() { time.Sleep(60 * time.Millisecond); close(indexDone) }()

	installed := prepareUsageFastPath(context.Background(), db, indexDone, logger, usageFastPathBounds{IndexWait: 5 * time.Second, ClassifyWait: 5 * time.Second}, classify)
	if classifiedBeforeIndexDone.Load() {
		t.Fatal("classification started while the index migration still held its maintenance write")
	}
	if !installed || !nosql.UsageIntegrityStats(db).Complete {
		t.Fatalf("classification must be installed before the quota preload (installed=%v)", installed)
	}
}

// 迁移迟迟不结束（首次升级的大库）时不能无限拖住启动：到上限后降级继续，由后台循环兜底。
func TestPrepareUsageFastPathDegradesWhenMigrationStalls(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	db := fastPathDB(t)
	never := make(chan struct{})
	start := time.Now()
	installed := prepareUsageFastPath(context.Background(), db, never, logger, usageFastPathBounds{IndexWait: 80 * time.Millisecond, ClassifyWait: time.Second},
		func(context.Context, *bolt.DB) error { return nil })
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("startup blocked %v on a stalled migration", d)
	}
	if installed {
		t.Fatal("must report not installed when the classification did not complete")
	}
}

// 停止信号在启动等待期间到来时立即返回。
func TestPrepareUsageFastPathStopsOnCancel(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	db := fastPathDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	prepareUsageFastPath(ctx, db, make(chan struct{}), logger, usageFastPathBounds{IndexWait: time.Minute, ClassifyWait: time.Minute},
		func(context.Context, *bolt.DB) error { return nil })
	if d := time.Since(start); d > time.Second {
		t.Fatalf("canceled startup waited %v", d)
	}
}
