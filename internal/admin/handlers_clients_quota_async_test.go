package admin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/quota"
)

// Issue #240：客户端保存（PUT）不再在响应路径上同步 Evaluate。
// Evaluate 会扫描该客户端约 31 天的用量窗口，对有额度组的客户端是秒级；而 PUT 载荷里能
// 影响额度判定的字段只有 job_level 与改名（额度计划在独立 store，不随该请求变化）。

// quotaProbe 记录 Evaluate / ScheduleRefresh 调用；block 非 nil 时 Evaluate 会一直阻塞，
// 用来证明响应不依赖它。
type quotaProbe struct {
	mu        sync.Mutex
	evaluates []string
	scopes    []quota.RefreshScope
	notes     int
	block     chan struct{}
}

func (q *quotaProbe) Status(string) quota.QuotaStatus    { return quota.QuotaStatus{} }
func (q *quotaProbe) RefreshStatus() quota.RefreshStatus { return quota.RefreshStatus{} }
func (q *quotaProbe) NoteConfigChange() uint64 {
	q.mu.Lock()
	q.notes++
	q.mu.Unlock()
	return 0
}
func (q *quotaProbe) Evaluate(name string) {
	q.mu.Lock()
	q.evaluates = append(q.evaluates, name)
	block := q.block
	q.mu.Unlock()
	if block != nil {
		<-block
	}
}
func (q *quotaProbe) ScheduleRefresh(scope quota.RefreshScope) {
	q.mu.Lock()
	q.scopes = append(q.scopes, scope)
	q.mu.Unlock()
}

func (q *quotaProbe) reset() {
	q.mu.Lock()
	q.evaluates, q.scopes, q.notes = nil, nil, 0
	q.mu.Unlock()
}

func (q *quotaProbe) snapshot() (evaluates []string, scopes []quota.RefreshScope, notes int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]string(nil), q.evaluates...), append([]quota.RefreshScope(nil), q.scopes...), q.notes
}

// newQuotaAsyncEnv 建立一个带 job_level=L1 的客户端 qc，并装上探针（建档产生的调用已清零）。
func newQuotaAsyncEnv(t *testing.T) (*revEnv, *quotaProbe) {
	t.Helper()
	e := newRevEnv(t, t.TempDir(), false)
	q := &quotaProbe{}
	e.h.quotaManager = q
	rec := e.do(http.MethodPost, "clients", "", config.Client{Name: "qc", AccessKey: "ck-old", AllowedTargets: []string{"target-a"}, JobLevel: "L1"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("seed client: %d %s", rec.Code, rec.Body.String())
	}
	q.reset()
	return e, q
}

// 只改 IP/ACL/access_key/项目组：额度判定没有任何输入变化，不得触发评估、代次推进或刷新。
func TestUpdateClientWithoutQuotaRelevantChangeSkipsQuotaWork(t *testing.T) {
	e, q := newQuotaAsyncEnv(t)

	rec := e.do(http.MethodPut, "clients/qc", "", config.Client{
		Name: "qc", AccessKey: "ck-new", AllowedTargets: []string{"target-b"}, IPAddress: "10.0.0.0/8", JobLevel: " L1 ", ProjectGroup: "pg",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("update: %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "quota_refresh") {
		t.Fatalf("unrelated update must not announce a quota refresh: %s", rec.Body.String())
	}
	evals, scopes, notes := q.snapshot()
	if len(evals) != 0 || len(scopes) != 0 || notes != 0 {
		t.Fatalf("unrelated update touched quota: evaluate=%v scopes=%v notes=%d", evals, scopes, notes)
	}
	// 访问控制仍在响应返回前生效。
	if _, ok := e.h.authStore.Authenticate("ck-new"); !ok {
		t.Fatal("new access key must authenticate right after the write")
	}
	if _, ok := e.h.authStore.Authenticate("ck-old"); ok {
		t.Fatal("old access key must stop authenticating right after the write")
	}
}

// job_level 变化：响应不等待 Evaluate（即使它永远阻塞），改为登记后台刷新。
func TestUpdateClientJobLevelChangeSchedulesBackgroundRefreshWithoutBlocking(t *testing.T) {
	e, q := newQuotaAsyncEnv(t)
	q.block = make(chan struct{}) // 若 handler 仍同步 Evaluate，下面的请求会卡死
	t.Cleanup(func() { close(q.block) })

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- e.do(http.MethodPut, "clients/qc", "", config.Client{
			Name: "qc", AccessKey: "ck-new", AllowedTargets: []string{"target-a"}, JobLevel: "L2",
		})
	}()
	var rec *httptest.ResponseRecorder
	select {
	case rec = <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("client PUT blocked on a synchronous quota Evaluate")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("update: %d %s", rec.Code, rec.Body.String())
	}
	if !jsonField(t, rec.Body.Bytes(), "quota_refresh", "pending") {
		t.Fatalf("job_level change must announce quota_refresh=pending: %s", rec.Body.String())
	}
	evals, scopes, _ := q.snapshot()
	if len(evals) != 0 {
		t.Fatalf("handler must not Evaluate synchronously, got %v", evals)
	}
	if len(scopes) != 1 || len(scopes[0].Clients) != 1 || scopes[0].Clients[0] != "qc" || scopes[0].All {
		t.Fatalf("want one per-client refresh for qc, got %+v", scopes)
	}
	if _, ok := e.h.authStore.Authenticate("ck-new"); !ok {
		t.Fatal("new access key must authenticate right after the write")
	}
}

// 改名：旧名要清理内存状态、新名要重新校准，两个名字都要进入刷新范围。
func TestUpdateClientRenameSchedulesRefreshForOldAndNewName(t *testing.T) {
	e, q := newQuotaAsyncEnv(t)

	rec := e.do(http.MethodPut, "clients/qc", "", config.Client{
		Name: "qc-renamed", AccessKey: "ck-old", AllowedTargets: []string{"target-a"}, JobLevel: "L1",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("rename: %d %s", rec.Code, rec.Body.String())
	}
	evals, scopes, _ := q.snapshot()
	if len(evals) != 0 {
		t.Fatalf("rename must not Evaluate synchronously, got %v", evals)
	}
	if len(scopes) != 1 {
		t.Fatalf("want one refresh, got %+v", scopes)
	}
	got := map[string]bool{}
	for _, c := range scopes[0].Clients {
		got[c] = true
	}
	if !got["qc"] || !got["qc-renamed"] {
		t.Fatalf("refresh scope must name old and new client, got %v", scopes[0].Clients)
	}
}

// 失败的更新（落库前被拒）不得触发任何额度工作。
func TestUpdateClientRejectedWriteSkipsQuotaWork(t *testing.T) {
	e, q := newQuotaAsyncEnv(t)

	rec := e.do(http.MethodPut, "clients/missing", "", config.Client{Name: "missing", AccessKey: "ck-x", JobLevel: "L2"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("update missing: %d %s", rec.Code, rec.Body.String())
	}
	evals, scopes, notes := q.snapshot()
	if len(evals) != 0 || len(scopes) != 0 || notes != 0 {
		t.Fatalf("rejected update touched quota: evaluate=%v scopes=%v notes=%d", evals, scopes, notes)
	}
}
