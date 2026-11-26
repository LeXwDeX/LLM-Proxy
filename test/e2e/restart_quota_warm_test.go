//go:build e2e

package e2e

import (
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/fakellm"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	"net/http/httptest"
)

// 重启后的就绪窗口内不得出现额度校准 503：
// 完整性分类须在额度预加载之前装好（预加载走窗口快路径），所有已有历史的客户端在
// 服务就绪后的第一个请求就应被准入，不依赖请求路径的 15s 校准等待。
func TestRestartQuotaCalibrationHasNoReadinessWindow503(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "proxy")
	build := exec.Command("go", "build", "-o", bin, "./cmd/proxy")
	build.Dir = repoRoot()
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build proxy: %v\n%s", err, out)
	}
	fake := fakellm.New(fakellm.Options{Name: "restart-warm"})
	upstream := httptest.NewServer(fake)
	defer upstream.Close()
	port, err := freePort()
	if err != nil {
		t.Fatal(err)
	}
	cfg := map[string]any{
		"server":        map[string]any{"bind": fmt.Sprintf("127.0.0.1:%d", port), "request_timeout_seconds": 30},
		"data_store":    map[string]any{"db_path": filepath.Join(dir, "proxy.db")},
		"admin_session": map[string]any{"cookie_name": "admin_sid", "secret": "restart-warm-secret", "ttl_seconds": 3600},
		"logging":       map[string]any{"level": "warn", "access_log": filepath.Join(dir, "access.log"), "error_log": filepath.Join(dir, "error.log")},
	}
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, mustJSON(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	logPath := filepath.Join(dir, "proxy.out")
	logs, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = logs.Close() })
	logText := func() string { raw, _ := os.ReadFile(logPath); return string(raw) }
	var proc *exec.Cmd
	stop := func() {
		if proc == nil {
			return
		}
		_ = proc.Process.Signal(os.Interrupt)
		done := make(chan error, 1)
		go func(p *exec.Cmd) { done <- p.Wait() }(proc)
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = proc.Process.Kill()
			<-done
		}
		proc = nil
	}
	t.Cleanup(stop)
	start := func() *world {
		proc = exec.Command(bin, "-config", cfgPath)
		proc.Dir = dir
		proc.Env = append(os.Environ(), "ADMIN_PASSWORD="+adminPassword, "LOG_LEVEL=warn", "UPSTREAM_ERROR_LOG_PATH="+filepath.Join(dir, "upstream-error.log"), "TRACE_STORE_ENABLED=false", "SERVER_BIND="+fmt.Sprintf("127.0.0.1:%d", port))
		proc.Stdout, proc.Stderr = logs, logs
		if err := proc.Start(); err != nil {
			t.Fatal(err)
		}
		if err := waitHealthy(base, 60*time.Second); err != nil {
			t.Fatalf("%v: %s", err, logText())
		}
		jar, _ := cookiejar.New(nil)
		admin := &http.Client{Jar: jar, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		resp, err := admin.PostForm(base+"/login", url.Values{"username": {"admin"}, "password": {adminPassword}})
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode >= 400 {
			t.Fatalf("login: %d", resp.StatusCode)
		}
		return &world{base: base, admin: admin}
	}
	w := start()
	adminCall := func(method, path string, body any, want int) []byte {
		t.Helper()
		code, raw := w.adminDo(method, path, body)
		if code != want {
			t.Fatalf("%s %s: HTTP %d want %d: %s", method, path, code, want, raw)
		}
		return raw
	}
	adminCall("POST", "/admin/data/job-levels", map[string]any{"name": "level", "coefficient_daily": 1}, 201)
	adminCall("POST", "/admin/data/targets", map[string]any{"name": "target", "endpoint_type": "openai", "endpoint": upstream.URL + "/openai/v1", "api_key": "fake-key", "usage_adapter": "standard", "model_mappings": []any{map[string]any{"upstream": "WarmModel", "fallback": "warm"}}}, 201)
	adminCall("POST", "/admin/data/pricing-templates", map[string]any{"target": "target", "model": "WarmModel", "mode": "flat", "base_rates": usage.CostRates{InputPer1MTokens: 2}}, 201)
	adminCall("POST", "/admin/data/model-groups", map[string]any{"name": "models", "daily_usd": 1000, "cost_coefficient": 1, "bindings": []any{map[string]any{"target": "target", "model": "WarmModel"}}}, 201)
	const clients = 12
	call := func(name string) (int, string) {
		t.Helper()
		body := `{"model":"warm","messages":[{"role":"user","content":"[[fake usage.in=1000 usage.out=0 usage.cached=0 usage.reasoning=0]]"}]}`
		req, err := http.NewRequest("POST", base+"/v1/chat/completions", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer sk-warm-"+name)
		req.Header.Set("Content-Type", "application/json")
		resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(raw)
	}
	for i := 0; i < clients; i++ {
		name := fmt.Sprintf("c%02d", i)
		adminCall("POST", "/admin/data/clients", map[string]any{"name": name, "access_key": "sk-warm-" + name, "job_level": "level", "allowed_targets": []string{"target"}}, 201)
		adminCall("PUT", "/admin/data/quota/"+name, map[string]any{"job_level": "level", "quota_groups": []any{map[string]any{"name": "models"}}}, 200)
		if code, raw := call(name); code != 200 {
			t.Fatalf("seed %s: %d %s", name, code, raw)
		}
	}
	// 等历史落库，使重启后的校准确有数据可读。
	time.Sleep(time.Second)
	stop()

	w = start()
	// 预加载必须全部走窗口快路径：不依赖「分类此刻仍有效」（启动后的首次保留期清理会让它
	// 暂时失效，下一轮重建恢复），而是看预加载期间的读取计数。
	metrics := decode(t, adminCall("GET", "/admin/metrics", nil, 200))
	fast, _ := dig(metrics, "usage_integrity/fast_reads").(float64)
	incomplete, _ := dig(metrics, "usage_integrity/fallback_reads/incomplete").(float64)
	if fast < clients || incomplete != 0 {
		t.Fatalf("quota preload must read through the installed classification: fast_reads=%v incomplete_fallbacks=%v; %s", fast, incomplete, logText())
	}
	for i := 0; i < clients; i++ {
		name := fmt.Sprintf("c%02d", i)
		if code, raw := call(name); code != 200 {
			t.Fatalf("first request of %s after restart: %d %s\n%s", name, code, raw, logText())
		}
	}
	if strings.Contains(logText(), "calibration timed out") {
		t.Fatalf("request-path calibration timed out after restart:\n%s", logText())
	}
}
