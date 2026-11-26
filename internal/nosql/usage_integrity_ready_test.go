package nosql

import (
	"context"
	"testing"
	"time"
)

// 启动时额度预加载要等分类首次安装后再跑，否则每个客户端校准都退回全库穷举扫描。
// WaitUsageIntegrityReady 在分类完成时立即返回 true，超时或取消返回 false，不阻塞写入。
func TestWaitUsageIntegrityReady(t *testing.T) {
	productionIntegrityMode(t)
	db, _, _ := integrityFixture(t)

	start := time.Now()
	if WaitUsageIntegrityReady(context.Background(), db, 60*time.Millisecond) {
		t.Fatal("ready reported before any classification was installed")
	}
	if d := time.Since(start); d < 50*time.Millisecond || d > 2*time.Second {
		t.Fatalf("timeout wait took %v, want about 60ms", d)
	}

	// 分类在等待期间完成：等待者应及时被唤醒并返回 true。
	go func() {
		time.Sleep(80 * time.Millisecond)
		_ = RunClassificationOnce(context.Background(), db)
	}()
	start = time.Now()
	if !WaitUsageIntegrityReady(context.Background(), db, 5*time.Second) {
		t.Fatal("not ready although classification completed within the wait")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("waiter woke %v after install, want prompt", d)
	}

	// 已经完成时立即返回；上下文取消时立即返回 false。
	if !WaitUsageIntegrityReady(context.Background(), db, time.Second) {
		t.Fatal("must report ready immediately once installed")
	}
	ForgetUsageIntegrityClassification(db)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if WaitUsageIntegrityReady(ctx, db, time.Minute) {
		t.Fatal("canceled context must not report ready")
	}
}
