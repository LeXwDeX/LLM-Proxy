package admin

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/quota"
)

// 这些用例固定「额度相关配置提交之后必须推进额度配置代次」：在途校准据此丢弃按提交前
// 配置算出的结果。删掉对应的推进调用，用例必须失败。

func newGenManager(t *testing.T) *quota.Manager {
	t.Helper()
	m, err := quota.New(quota.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.FlushRefresh)
	return m
}

func genOf(m *quota.Manager) uint64 { return m.RefreshStatus().ConfigGeneration }

func TestCreateJobLevelAdvancesGenerationAndSchedulesRefresh(t *testing.T) {
	e := newIssue94Env(t)
	m := newGenManager(t)
	e.h.quotaManager = m
	before := genOf(m)
	rec := e.do(t, RoleAdmin, "operator", http.MethodPost, "/data/job-levels", `{"name":"P9","coefficient_daily":2}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	if genOf(m) <= before {
		t.Fatal("job level create did not advance the quota config generation")
	}
	if !m.RefreshStatus().Pending && m.RefreshStatus().Completed == 0 {
		t.Fatal("job level create did not schedule a refresh")
	}
}

func TestTargetCommitAdvancesGeneration(t *testing.T) {
	e := newRevEnv(t, t.TempDir(), false)
	m := newGenManager(t)
	e.h.quotaManager = m
	before := genOf(m)
	// 暂停目标走 commitConfigChange，且不排期刷新：代次只能由提交管道推进。
	if rec := e.do(http.MethodPut, "targets/target-b/pause", "", nil); rec.Code != http.StatusOK {
		t.Fatalf("pause: %d %s", rec.Code, rec.Body.String())
	}
	if genOf(m) != before+1 {
		t.Fatalf("generation %d -> %d, want +1 from commitConfigChange", before, genOf(m))
	}
}

func TestConfigReloadAdvancesGeneration(t *testing.T) {
	e := newRevEnv(t, t.TempDir(), false)
	m := newGenManager(t)
	e.h.quotaManager = m
	before := genOf(m)
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "http://example.com/config/reload", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("reload: %d %s", rec.Code, rec.Body.String())
	}
	if genOf(m) <= before {
		t.Fatal("config reload did not advance the quota config generation")
	}
}

func TestClientWritesAdvanceGeneration(t *testing.T) {
	e := newIssue94Env(t)
	m := newGenManager(t)
	e.h.quotaManager = m
	steps := []struct {
		name, method, path, body string
	}{
		{"create", http.MethodPost, "/data/clients", `{"name":"gen-c","access_key":"sk-gen-client-key-123"}`},
		{"update", http.MethodPut, "/data/clients/gen-c", `{"name":"gen-c","access_key":"sk-gen-client-key-456"}`},
		{"update quota", http.MethodPut, "/data/quota/gen-c", `{}`},
		{"delete", http.MethodDelete, "/data/clients/gen-c", ""},
	}
	for _, s := range steps {
		before := genOf(m)
		rec := e.do(t, RoleAdmin, "operator", s.method, s.path, s.body)
		if rec.Code >= 300 {
			t.Fatalf("%s: %d %s", s.name, rec.Code, rec.Body.String())
		}
		if genOf(m) <= before {
			t.Errorf("%s did not advance the quota config generation", s.name)
		}
	}
}
