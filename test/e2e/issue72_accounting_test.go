//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/fakellm"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	bolt "go.etcd.io/bbolt"
)

// This test owns a separate proxy and database. The sidecar represents a
// historical rejected record, not a failure injected into a live database.
func TestIssue72SidecarAccountingRestart(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "proxy")
	build := exec.Command("go", "build", "-o", bin, "./cmd/proxy")
	build.Dir = repoRoot()
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build proxy: %v\n%s", err, out)
	}
	fake := fakellm.New(fakellm.Options{Name: "issue72"})
	upstream := httptest.NewServer(fake)
	defer upstream.Close()
	port, err := freePort()
	if err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dir, "proxy.db")
	cfg := map[string]any{
		"server":        map[string]any{"bind": fmt.Sprintf("127.0.0.1:%d", port), "request_timeout_seconds": 30},
		"data_store":    map[string]any{"db_path": dbPath},
		"admin_session": map[string]any{"cookie_name": "admin_sid", "secret": "issue72-test-secret", "ttl_seconds": 3600},
		"logging":       map[string]any{"level": "warn", "access_log": filepath.Join(dir, "access.log"), "error_log": filepath.Join(dir, "error.log")},
	}
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, mustJSON(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	var proc *exec.Cmd
	logPath := filepath.Join(dir, "proxy.out")
	logs, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = logs.Close() })
	logText := func() string { raw, _ := os.ReadFile(logPath); return string(raw) }
	stop := func() {
		if proc == nil {
			return
		}
		_ = proc.Process.Signal(os.Interrupt)
		done := make(chan error, 1)
		go func(p *exec.Cmd) { done <- p.Wait() }(proc)
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("proxy shutdown: %v; %s", err, logText())
			}
		case <-time.After(10 * time.Second):
			_ = proc.Process.Kill()
			<-done
			t.Errorf("proxy shutdown timed out; %s", logText())
		}
		proc = nil
	}
	t.Cleanup(stop)
	start := func() *world {
		proc = exec.Command(bin, "-config", cfgPath)
		proc.Dir = dir
		proc.Env = append(os.Environ(), "ADMIN_PASSWORD="+adminPassword, "LOG_LEVEL=warn", "UPSTREAM_ERROR_LOG_PATH="+filepath.Join(dir, "upstream-error.log"), "TRACE_STORE_ENABLED=false")
		proc.Stdout, proc.Stderr = logs, logs
		if err := proc.Start(); err != nil {
			t.Fatal(err)
		}
		if err := waitHealthy(base, 30*time.Second); err != nil {
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
	w72 := start()
	admin := func(method, path string, body any, want int) []byte {
		t.Helper()
		code, raw := w72.adminDo(method, path, body)
		if code != want {
			t.Fatalf("%s %s: HTTP %d want %d: %s", method, path, code, want, raw)
		}
		return raw
	}
	admin("POST", "/admin/data/job-levels", map[string]any{"name": "level", "coefficient_daily": 1}, 201)
	admin("POST", "/admin/data/targets", map[string]any{"name": "target", "endpoint_type": "openai", "endpoint": upstream.URL + "/openai/v1", "api_key": "fake-key", "usage_adapter": "standard", "model_mappings": []any{map[string]any{"upstream": "Issue72Model", "fallback": "issue72"}}}, 201)
	admin("POST", "/admin/data/pricing-templates", map[string]any{"target": "target", "model": "Issue72Model", "mode": "flat", "base_rates": usage.CostRates{InputPer1MTokens: 2}}, 201)
	admin("POST", "/admin/data/model-groups", map[string]any{"name": "models", "daily_usd": 100, "cost_coefficient": 1, "bindings": []any{map[string]any{"target": "target", "model": "Issue72Model"}}}, 201)
	for _, name := range []string{"alice", "bob"} {
		admin("POST", "/admin/data/clients", map[string]any{"name": name, "access_key": "sk-issue72-" + name, "job_level": "level", "allowed_targets": []string{"target"}}, 201)
		admin("PUT", "/admin/data/quota/"+name, map[string]any{"job_level": "level", "quota_groups": []any{map[string]any{"name": "models"}}}, 200)
	}
	callClient := func(name string) (int, string) {
		t.Helper()
		body := `{"model":"issue72","messages":[{"role":"user","content":"[[fake usage.in=1000000 usage.out=0 usage.cached=0 usage.reasoning=0]]"}]}`
		req, err := http.NewRequest("POST", base+"/v1/chat/completions", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer sk-issue72-"+name)
		req.Header.Set("Content-Type", "application/json")
		resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, string(raw)
	}
	for _, name := range []string{"alice", "bob"} {
		if code, raw := callClient(name); code != 200 {
			t.Fatalf("seed %s: %d %s", name, code, raw)
		}
	}
	readEvents := func(name string) []usage.Event {
		t.Helper()
		var result struct {
			Events []usage.Event `json:"events"`
		}
		if err := json.Unmarshal(admin("GET", "/admin/data/usage/events?client_name="+name, nil, 200), &result); err != nil {
			t.Fatal(err)
		}
		return result.Events
	}
	for _, name := range []string{"alice", "bob"} {
		deadline := time.Now().Add(10 * time.Second)
		for len(readEvents(name)) != 1 {
			if time.Now().After(deadline) {
				t.Fatalf("%s ledger event not persisted", name)
			}
			time.Sleep(100 * time.Millisecond)
		}
		var result usage.AggregateResult
		if err := json.Unmarshal(admin("GET", "/admin/data/usage/aggregate?group_by=hour&client_name="+name, nil, 200), &result); err != nil {
			t.Fatal(err)
		}
		if result.Totals.Requests != 1 || result.Totals.InputTokens != 1000000 || math.Abs(result.Totals.EstimatedCost-2) > 1e-9 {
			t.Fatalf("%s healthy aggregate: %+v", name, result.Totals)
		}
		status := decode(t, admin("GET", "/admin/data/quota/"+name, nil, 200))
		if dig(status, "calibration_error") != nil || dig(status, "groups/models/quotas/daily/used") != float64(2) {
			t.Fatalf("%s healthy quota: %+v", name, status)
		}
	}
	event := readEvents("alice")[0]
	bobEvent := readEvents("bob")[0]
	var aliceEvidenceID string
	if event.RecordID == "" || event.InputTokens != 1000000 {
		t.Fatalf("unexpected primary event: %+v", event)
	}
	stop()
	// Supply an older rejected sidecar entry with a distinct identity. The
	// production startup imports this evidence; it is not a fabricated ledger row.
	event.RecordID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaa0072"
	line := mustJSON(event)
	sidecar := append(mustJSON(map[string]any{"rejected_at": time.Now().UTC(), "reason": "identity_unverified", "record_id": event.RecordID, "client": "alice", "line": line}), '\n')
	if err := os.WriteFile(dbPath+".usage-journal.jsonl.rejected", sidecar, 0o600); err != nil {
		t.Fatal(err)
	}
	for restart := 0; restart < 2; restart++ {
		w72 = start()
		if got := readEvents("alice"); len(got) != 1 || got[0].RecordID == event.RecordID {
			t.Fatalf("sidecar entered primary ledger: %+v", got)
		}
		admin("GET", "/admin/data/usage/aggregate?group_by=hour&client_name=alice", nil, 500)
		var other usage.AggregateResult
		if err := json.Unmarshal(admin("GET", "/admin/data/usage/aggregate?group_by=hour&client_name=bob", nil, 200), &other); err != nil {
			t.Fatal(err)
		}
		if other.Totals.Requests != 1+int64(restart) || math.Abs(other.Totals.EstimatedCost-float64(2*(1+restart))) > 1e-9 {
			t.Fatalf("unrelated aggregate: %+v", other.Totals)
		}
		status := decode(t, admin("GET", "/admin/data/quota/alice", nil, 200))
		if dig(status, "calibration_error") == nil || dig(status, "calibration_error") == "" {
			t.Fatalf("missing incomplete calibration: %+v", status)
		}
		var evidence struct {
			Entries []struct {
				ID       string `json:"id"`
				Version  string `json:"version"`
				Resolved bool   `json:"resolved"`
			} `json:"entries"`
		}
		if err := json.Unmarshal(admin("GET", "/admin/data/usage/unresolved", nil, 200), &evidence); err != nil {
			t.Fatal(err)
		}
		if len(evidence.Entries) != 1 || evidence.Entries[0].Resolved {
			t.Fatalf("unexpected evidence: %+v", evidence)
		}
		aliceEvidenceID = evidence.Entries[0].ID
		if restart == 0 {
			entry := evidence.Entries[0]
			result := decode(t, admin("POST", "/admin/data/usage/recheck", map[string]any{"evidence_id": entry.ID, "version": entry.Version, "operation_id": "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbb072"}, 200))
			if result["status"] != "insufficient_evidence" {
				t.Fatalf("unconfirmed record was resolved: %+v", result)
			}
		}
		bobStatus := decode(t, admin("GET", "/admin/data/quota/bob", nil, 200))
		if dig(bobStatus, "calibration_error") != nil || dig(bobStatus, "groups/models/quotas/daily/used") != float64(2*(1+restart)) {
			t.Fatalf("unrelated quota: %+v", bobStatus)
		}
		seq := fake.LastSeq()
		if code, raw := callClient("alice"); code != 503 || !strings.Contains(raw, "accounting_unavailable") {
			t.Fatalf("affected request: %d %s", code, raw)
		}
		if fake.LastSeq() != seq {
			t.Fatal("affected request reached fakellm")
		}
		if code, raw := callClient("bob"); code != 200 {
			t.Fatalf("unrelated request: %d %s", code, raw)
		}
		if fake.LastSeq() == seq {
			t.Fatal("unrelated request did not reach fakellm")
		}
		stop()
		if got, err := os.ReadFile(dbPath + ".usage-journal.jsonl.rejected"); err != nil || string(got) != string(sidecar) {
			t.Fatalf("rejected evidence changed: %v", err)
		}
	}
	// Add a duplicate of an actually committed event as unresolved evidence to
	// the stopped test database. Its original bytes and ledger stay intact.
	db, err := bolt.Open(dbPath, 0o600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	original := mustJSON(bobEvent)
	err = db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("usage_unresolved_events_v1"))
		if b == nil {
			return fmt.Errorf("unresolved bucket missing")
		}
		return b.Put([]byte("e2e-matching-bob"), mustJSON(map[string]any{"event": bobEvent, "original": original}))
	})
	closeErr := db.Close()
	if err != nil || closeErr != nil {
		t.Fatalf("seed evidence: %v / %v", err, closeErr)
	}
	w72 = start()
	var evidence struct {
		Entries []struct {
			ID       string `json:"id"`
			Version  string `json:"version"`
			Resolved bool   `json:"resolved"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(admin("GET", "/admin/data/usage/unresolved", nil, 200), &evidence); err != nil {
		t.Fatal(err)
	}
	var selected struct {
		ID       string `json:"id"`
		Version  string `json:"version"`
		Resolved bool   `json:"resolved"`
	}
	for _, item := range evidence.Entries {
		if item.ID != aliceEvidenceID && !item.Resolved {
			selected = item
		}
	}
	if len(evidence.Entries) != 2 || selected.ID == "" {
		t.Fatalf("positive fixture missing: %+v", evidence)
	}
	admin("GET", "/admin/data/usage/aggregate?group_by=hour&client_name=bob", nil, 500)
	request := map[string]any{"evidence_id": selected.ID, "version": selected.Version, "operation_id": "cccccccc-cccc-4ccc-8ccc-cccccccccc72"}
	result := decode(t, admin("POST", "/admin/data/usage/recheck", request, 200))
	if result["accounting_verified"] != true || result["accounting_restored"] != true {
		t.Fatalf("positive recheck: %+v", result)
	}
	var bob usage.AggregateResult
	if err := json.Unmarshal(admin("GET", "/admin/data/usage/aggregate?group_by=hour&client_name=bob", nil, 200), &bob); err != nil {
		t.Fatal(err)
	}
	if bob.Totals.Requests != 3 || math.Abs(bob.Totals.EstimatedCost-6) > 1e-9 {
		t.Fatalf("recheck changed accounting: %+v", bob.Totals)
	}
	admin("GET", "/admin/data/usage/aggregate?group_by=hour&client_name=alice", nil, 500)
	admin("PUT", "/admin/data/quota/bob", map[string]any{"job_level": "level", "quota_groups": []any{map[string]any{"name": "models"}}}, 200)
	status := decode(t, admin("GET", "/admin/data/quota/bob", nil, 200))
	if dig(status, "calibration_error") != nil || dig(status, "groups/models/quotas/daily/used") != float64(6) {
		t.Fatalf("restored quota: %+v", status)
	}
	stop()
	w72 = start()
	retry := decode(t, admin("POST", "/admin/data/usage/recheck", request, 200))
	if retry["accounting_verified"] != true || retry["accounting_restored"] != true {
		t.Fatalf("restart retry: %+v", retry)
	}
	admin("GET", "/admin/data/usage/aggregate?group_by=hour&client_name=alice", nil, 500)
	stop()

	// Keep the earlier unresolved evidence while testing independent quota-cell
	// damage from real, successfully recorded requests.
	w72 = start()
	for _, name := range []string{"carol", "ellen", "dave"} {
		admin("POST", "/admin/data/clients", map[string]any{"name": name, "access_key": "sk-issue72-" + name, "job_level": "level", "allowed_targets": []string{"target"}}, 201)
		admin("PUT", "/admin/data/quota/"+name, map[string]any{"job_level": "level", "quota_groups": []any{map[string]any{"name": "models"}}}, 200)
		if code, raw := callClient(name); code != 200 {
			t.Fatalf("direct quota seed %s: %d %s", name, code, raw)
		}
	}
	directEvents := make(map[string]usage.Event)
	for _, name := range []string{"carol", "ellen", "dave"} {
		deadline := time.Now().Add(10 * time.Second)
		for len(readEvents(name)) != 1 {
			if time.Now().After(deadline) {
				t.Fatalf("direct quota %s ledger event not persisted", name)
			}
			time.Sleep(100 * time.Millisecond)
		}
		directEvents[name] = readEvents(name)[0]
		var total usage.AggregateResult
		if err := json.Unmarshal(admin("GET", "/admin/data/usage/aggregate?group_by=hour&client_name="+name, nil, 200), &total); err != nil {
			t.Fatal(err)
		}
		status := decode(t, admin("GET", "/admin/data/quota/"+name, nil, 200))
		if total.Totals.Requests != 1 || total.Totals.InputTokens != 1000000 || math.Abs(total.Totals.EstimatedCost-2) > 1e-9 || dig(status, "groups/models/quotas/daily/used") != float64(2) || dig(status, "calibration_error") != nil {
			t.Fatalf("direct quota healthy %s: total=%+v quota=%+v", name, total.Totals, status)
		}
	}
	stop()
	for i, tc := range []struct{ name, damage string }{{"carol", "missing"}, {"ellen", "null"}} {
		event := directEvents[tc.name]
		// Names here contain no escaped key characters; match the exact cell
		// produced by the live request rather than scanning or replacing a bucket.
		key := []byte(event.Timestamp.UTC().Truncate(time.Hour).Format("2006-01-02T15") + "|" + tc.name + "|" + strings.ToLower(event.Target) + "|" + strings.ToLower(event.Model))
		db, err := bolt.Open(dbPath, 0o600, &bolt.Options{Timeout: time.Second})
		if err != nil {
			t.Fatal(err)
		}
		err = db.Update(func(tx *bolt.Tx) error {
			b := tx.Bucket([]byte("quota_usage_agg_hourly"))
			if b == nil || len(b.Get(key)) == 0 {
				return fmt.Errorf("missing healthy cell %q before damage", key)
			}
			var cell struct {
				InputTokens int64 `json:"input_tokens"`
			}
			if err := json.Unmarshal(b.Get(key), &cell); err != nil || cell.InputTokens != 1000000 {
				return fmt.Errorf("unexpected healthy cell %q: %+v (%v)", key, cell, err)
			}
			if tc.damage == "missing" {
				return b.Delete(key)
			}
			return b.Put(key, []byte("null"))
		})
		closeErr := db.Close()
		if err != nil || closeErr != nil {
			t.Fatalf("damage %s cell: %v / %v", tc.damage, err, closeErr)
		}
		w72 = start()
		// No replay, write, or retry is used to trigger integrity detection.
		if got := readEvents(tc.name); len(got) != 1 || got[0].RecordID != event.RecordID || got[0].InputTokens != 1000000 {
			t.Fatalf("%s damaged cell changed primary ledger: %+v", tc.damage, got)
		}
		admin("GET", "/admin/data/usage/aggregate?group_by=hour&client_name="+tc.name, nil, 500)
		status := decode(t, admin("GET", "/admin/data/quota/"+tc.name, nil, 200))
		if dig(status, "calibration_error") == nil || dig(status, "calibration_error") == "" {
			t.Fatalf("%s missing quota integrity error: %+v", tc.damage, status)
		}
		var other usage.AggregateResult
		if err := json.Unmarshal(admin("GET", "/admin/data/usage/aggregate?group_by=hour&client_name=dave", nil, 200), &other); err != nil {
			t.Fatal(err)
		}
		if other.Totals.Requests != int64(1+i) || math.Abs(other.Totals.EstimatedCost-float64(2*(1+i))) > 1e-9 {
			t.Fatalf("%s unrelated cost: %+v", tc.damage, other.Totals)
		}
		otherStatus := decode(t, admin("GET", "/admin/data/quota/dave", nil, 200))
		if dig(otherStatus, "calibration_error") != nil || dig(otherStatus, "groups/models/quotas/daily/used") != float64(2*(1+i)) {
			t.Fatalf("%s unrelated quota: %+v", tc.damage, otherStatus)
		}
		seq := fake.LastSeq()
		if code, raw := callClient(tc.name); code != 503 || !strings.Contains(raw, "accounting_unavailable") {
			t.Fatalf("%s affected admission: %d %s", tc.damage, code, raw)
		}
		if fake.LastSeq() != seq {
			t.Fatalf("%s affected request reached fakellm", tc.damage)
		}
		if code, raw := callClient("dave"); code != 200 || fake.LastSeq() == seq {
			t.Fatalf("%s unrelated admission: %d %s", tc.damage, code, raw)
		}
		admin("GET", "/admin/data/usage/aggregate?group_by=hour&client_name=alice", nil, 500)
		stop()
		db, err = bolt.Open(dbPath, 0o600, &bolt.Options{ReadOnly: true, Timeout: time.Second})
		if err != nil {
			t.Fatal(err)
		}
		err = db.View(func(tx *bolt.Tx) error {
			b := tx.Bucket([]byte("quota_usage_agg_hourly"))
			if b == nil {
				return fmt.Errorf("quota bucket disappeared")
			}
			value := b.Get(key)
			if (tc.damage == "missing" && value != nil) || (tc.damage == "null" && string(value) != "null") {
				return fmt.Errorf("damage %s was changed to %q", tc.damage, value)
			}
			return nil
		})
		closeErr = db.Close()
		if err != nil || closeErr != nil {
			t.Fatalf("read back %s evidence: %v / %v", tc.damage, err, closeErr)
		}
		if got, err := os.ReadFile(dbPath + ".usage-journal.jsonl.rejected"); err != nil || string(got) != string(sidecar) {
			t.Fatalf("%s changed old sidecar: %v", tc.damage, err)
		}
	}
}
