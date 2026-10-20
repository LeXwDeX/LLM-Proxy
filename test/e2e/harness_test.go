//go:build e2e

// Package e2e drives the real proxy binary against in-process fake LLM
// providers (internal/fakellm). Nothing here reaches a real model: every
// upstream is a fakellm instance, every client is this test.
//
// Run with: go test -tags=e2e ./test/e2e/ (or make e2e).
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/fakellm"
)

const adminPassword = "e2e-admin-password-not-secret"

// world is the shared proxy process and fake providers.
type world struct {
	base  string
	admin *http.Client
	fakeA *fakellm.Server
	fakeB *fakellm.Server
	urlA  string
	urlB  string
	logs  string
}

var (
	w     *world
	names atomic.Int64
)

func TestMain(m *testing.M) {
	code, err := run(m)
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e setup:", err)
		os.Exit(1)
	}
	os.Exit(code)
}

func repoRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func run(m *testing.M) (int, error) {
	dir, err := os.MkdirTemp("", "llms-e2e-")
	if err != nil {
		return 0, err
	}
	defer os.RemoveAll(dir)

	bin := filepath.Join(dir, "llms-proxy")
	build := exec.Command("go", "build", "-o", bin, "./cmd/proxy")
	build.Dir = repoRoot()
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	if err := build.Run(); err != nil {
		return 0, fmt.Errorf("build proxy: %w", err)
	}

	fakeA := fakellm.New(fakellm.Options{Name: "fake-a"})
	fakeB := fakellm.New(fakellm.Options{Name: "fake-b"})
	srvA, srvB := httptest.NewServer(fakeA), httptest.NewServer(fakeB)
	defer srvA.Close()
	defer srvB.Close()

	port, err := freePort()
	if err != nil {
		return 0, err
	}
	cfg := map[string]any{
		"server":        map[string]any{"bind": fmt.Sprintf("127.0.0.1:%d", port), "request_timeout_seconds": 30},
		"data_store":    map[string]any{"db_path": filepath.Join(dir, "proxy.db")},
		"admin_session": map[string]any{"cookie_name": "admin_sid", "secret": "e2e-session-secret", "ttl_seconds": 3600},
		"logging":       map[string]any{"level": "warn", "access_log": filepath.Join(dir, "access.log"), "error_log": filepath.Join(dir, "error.log")},
	}
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, mustJSON(cfg), 0o600); err != nil {
		return 0, err
	}
	logPath := filepath.Join(dir, "proxy.out")
	logFile, err := os.Create(logPath)
	if err != nil {
		return 0, err
	}
	defer logFile.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	proc := exec.CommandContext(ctx, bin, "-config", cfgPath)
	proc.Dir = dir
	proc.Env = append(os.Environ(), "ADMIN_PASSWORD="+adminPassword, "LOG_LEVEL=warn",
		"UPSTREAM_ERROR_LOG_PATH="+filepath.Join(dir, "upstream-error.log"), "TRACE_STORE_ENABLED=false")
	proc.Stdout, proc.Stderr = logFile, logFile
	if err := proc.Start(); err != nil {
		return 0, err
	}
	defer func() {
		_ = proc.Process.Signal(os.Interrupt)
		done := make(chan struct{})
		go func() { _ = proc.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			cancel()
		}
	}()

	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	if err := waitHealthy(base, 30*time.Second); err != nil {
		out, _ := os.ReadFile(logPath)
		return 0, fmt.Errorf("%w; proxy output:\n%s", err, out)
	}
	jar, _ := cookiejar.New(nil)
	admin := &http.Client{Jar: jar, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := admin.PostForm(base+"/login", url.Values{"username": {"admin"}, "password": {adminPassword}, "next": {"/admin"}})
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		return 0, fmt.Errorf("admin login: HTTP %d", resp.StatusCode)
	}
	w = &world{base: base, admin: admin, fakeA: fakeA, fakeB: fakeB, urlA: srvA.URL, urlB: srvB.URL, logs: logPath}
	if code, body := w.adminDo("POST", "/admin/data/job-levels", map[string]any{"name": "e2e-level",
		"coefficient_daily": 0, "coefficient_weekly": 0, "coefficient_monthly": 0}); code >= 300 {
		return 0, fmt.Errorf("create job level: %d %s", code, body)
	}
	code := m.Run()
	if code != 0 {
		out, _ := os.ReadFile(logPath)
		fmt.Fprintf(os.Stderr, "---- proxy output (tail) ----\n%s\n", tail(out, 6000))
	}
	return code, nil
}

func tail(b []byte, n int) []byte {
	if len(b) > n {
		return b[len(b)-n:]
	}
	return b
}

func waitHealthy(base string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(base + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("proxy not healthy at %s", base)
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func (w *world) adminDo(method, path string, body any) (int, []byte) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(mustJSON(body))
	}
	req, _ := http.NewRequest(method, w.base+path, rd)
	req.Header.Set("Content-Type", "application/json")
	resp, err := w.admin.Do(req)
	if err != nil {
		return 0, []byte(err.Error())
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func uniq(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, names.Add(1))
}

// target creates a target through the admin API and removes it afterwards.
// spec must not contain "name"; the generated name is returned. Generic fake
// targets explicitly use standard; only callers testing vendor patches override it.
func target(t *testing.T, spec map[string]any) string {
	t.Helper()
	name := uniq("e2e-t")
	spec["name"] = name
	if _, supplied := spec["usage_adapter"]; !supplied {
		spec["usage_adapter"] = "standard"
	}
	if code, body := w.adminDo("POST", "/admin/data/targets", spec); code >= 300 {
		t.Fatalf("create target: %d %s", code, body)
	}
	t.Cleanup(func() { w.adminDo("DELETE", "/admin/data/targets/"+name, nil) })
	return name
}

// pool creates a pool.
func pool(t *testing.T) string {
	t.Helper()
	name := uniq("e2e-p")
	if code, body := w.adminDo("POST", "/admin/data/pools", map[string]any{"name": name, "description": "e2e"}); code >= 300 {
		t.Fatalf("create pool: %d %s", code, body)
	}
	t.Cleanup(func() { w.adminDo("DELETE", "/admin/data/pools/"+name, nil) })
	return name
}

// client creates a client allowed to use the given targets or pools and
// returns its access key.
func client(t *testing.T, targets, pools []string) (string, string) {
	t.Helper()
	name := uniq("e2e-c")
	key := "sk-e2e-" + name
	spec := map[string]any{"name": name, "access_key": key, "job_level": "e2e-level"}
	if len(targets) > 0 {
		spec["allowed_targets"] = targets
	}
	if len(pools) > 0 {
		spec["allowed_pools"] = pools
	}
	if code, body := w.adminDo("POST", "/admin/data/clients", spec); code >= 300 {
		t.Fatalf("create client: %d %s", code, body)
	}
	t.Cleanup(func() { w.adminDo("DELETE", "/admin/data/clients/"+name, nil) })
	return name, key
}

// call sends a client request through the proxy.
func call(t *testing.T, key, method, path string, body any, hdr map[string]string) (*http.Response, []byte) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		if s, ok := body.(string); ok {
			rd = strings.NewReader(s)
		} else {
			rd = bytes.NewReader(mustJSON(body))
		}
	}
	req, _ := http.NewRequest(method, w.base+path, rd)
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s %s read: %v", method, path, err)
	}
	return resp, b
}

func decode(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("decode %q: %v", b, err)
	}
	return m
}

func dig(v any, path string) any {
	for _, seg := range strings.Split(path, "/") {
		switch x := v.(type) {
		case map[string]any:
			v = x[seg]
		case []any:
			i := 0
			for _, r := range seg {
				i = i*10 + int(r-'0')
			}
			if i >= len(x) {
				return nil
			}
			v = x[i]
		default:
			return nil
		}
	}
	return v
}

// usageEvent is the subset of usage.Event the tests assert on.
type usageEvent struct {
	ClientName              string `json:"client_name"`
	Target                  string `json:"target"`
	Model                   string `json:"model"`
	Path                    string `json:"path"`
	StatusCode              int    `json:"status_code"`
	UsageStatus             string `json:"usage_status"`
	ResponseStatus          string `json:"response_status"`
	FinishReason            string `json:"finish_reason"`
	UsageAdapter            string `json:"usage_adapter"`
	UsageDiagnostic         string `json:"usage_diagnostic"`
	CostIncomplete          bool   `json:"cost_incomplete"`
	InputTokens             int64  `json:"input_tokens"`
	OutputTokens            int64  `json:"output_tokens"`
	CachedTokens            int64  `json:"cached_tokens"`
	ExplicitCacheReadTokens int64  `json:"explicit_cache_read_tokens"`
	CacheCreationTokens     int64  `json:"cache_creation_tokens"`
	CacheCreation5m         int64  `json:"cache_creation_5m_tokens"`
	CacheCreation1h         int64  `json:"cache_creation_1h_tokens"`
	ReasoningTokens         int64  `json:"reasoning_tokens"`
	ImageInputTokens        int64  `json:"image_input_tokens"`
	CachedImageInputTokens  int64  `json:"cached_image_input_tokens"`
	ImageOutputTokens       int64  `json:"image_output_tokens"`
}

// usageFor polls the usage events of one client until want events exist.
func usageFor(t *testing.T, clientName string, want int) []usageEvent {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var got []usageEvent
	for time.Now().Before(deadline) {
		q := url.Values{"from": {time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)},
			"to": {time.Now().Add(10 * time.Minute).UTC().Format(time.RFC3339)}, "limit": {"1000"}}
		code, body := w.adminDo("GET", "/admin/data/usage/events?"+q.Encode(), nil)
		if code != 200 {
			t.Fatalf("usage events: %d %s", code, body)
		}
		var page struct {
			Events []usageEvent `json:"events"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			t.Fatalf("usage events decode: %v", err)
		}
		got = got[:0]
		for _, ev := range page.Events {
			if ev.ClientName == clientName {
				got = append(got, ev)
			}
		}
		if len(got) >= want {
			for _, ev := range got {
				t.Logf("usage event: %+v", ev)
			}
			return got
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("client %s: %d usage events, want %d", clientName, len(got), want)
	return nil
}

// lastRecord returns the newest fake journal entry whose path has suffix.
func lastRecord(t *testing.T, fake *fakellm.Server, suffix string) fakellm.Record {
	t.Helper()
	recs := fake.Requests()
	for i := len(recs) - 1; i >= 0; i-- {
		if strings.HasSuffix(recs[i].Path, suffix) {
			return recs[i]
		}
	}
	var paths []string
	for _, r := range recs[max(0, len(recs)-6):] {
		paths = append(paths, r.Method+" "+r.Path)
	}
	t.Fatalf("fake saw no request ending in %s; recent: %v", suffix, paths)
	return fakellm.Record{}
}

// keyStatus is the admin view of one key of a target.
type keyState struct {
	Index         int       `json:"index"`
	Exhausted     bool      `json:"exhausted"`
	ExhaustReason string    `json:"exhaust_reason"`
	CooldownEnd   time.Time `json:"cooldown_end"`
	Status        string    `json:"status"`
}

func keyStatus(t *testing.T, targetName string, index int) keyState {
	t.Helper()
	code, body := w.adminDo("GET", "/admin/data/targets", nil)
	if code != 200 {
		t.Fatalf("list targets: %d %s", code, body)
	}
	var page struct {
		Targets []struct {
			Name        string     `json:"name"`
			KeyStatuses []keyState `json:"key_statuses"`
		} `json:"targets"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatalf("decode targets: %v", err)
	}
	for _, tg := range page.Targets {
		if tg.Name == targetName {
			for _, ks := range tg.KeyStatuses {
				if ks.Index == index {
					return ks
				}
			}
		}
	}
	t.Fatalf("no key %d status for target %s", index, targetName)
	return keyState{}
}
