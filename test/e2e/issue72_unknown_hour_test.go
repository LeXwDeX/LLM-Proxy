//go:build e2e

package e2e

// Issue #72 requires usage identities that were never attested to fail closed:
// a raw ledger row without its identity evidence must not be treated as zero,
// as complete, or as newly valid usage. The sidecar test injects rejected
// historical evidence; this test owns a separate proxy and database and
// injects an *unknown* raw row directly into usage_events while the proxy is
// stopped. No raw-owner attestation, client index entry, hourly aggregation or
// quota aggregation is written for it. (Live rows in a tracked database are
// attested by the raw owner, client index and both cells; prune receipts and
// fresh attestations only exist for older or untracked ledgers.)
//
// Two windows are covered:
//
//   - current hour: the row is inside both the default aggregate window
//     (from zero, to now+1h) and the quota calibration window (period start to
//     now), so the default aggregate, quota calibration and admission must all
//     fail closed.
//   - future hour (+48h): today's default windows do not include the row. The
//     current contract is window-scoped, so today's totals and admission stay
//     healthy and unchanged -- Issue #72 acceptance also requires unrelated
//     time windows not to be blocked -- while the window that does include the
//     row (an explicit from/to aggregate) fails closed instead of reporting
//     zero or a complete total.

import (
	"bytes"
	"encoding/hex"
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

const issue72UnknownModel = "Issue72Model"

func TestIssue72UnknownHourIdentityRestart(t *testing.T) {
	t.Run("current_hour", func(t *testing.T) { runIssue72UnknownHourScenario(t, false) })
	t.Run("future_hour", func(t *testing.T) { runIssue72UnknownHourScenario(t, true) })
}

// issue72UnknownEnv owns one proxy process and bbolt file so the ledger can be
// edited between restarts, like a persisted database from an older version.
type issue72UnknownEnv struct {
	t        *testing.T
	dir      string
	bin      string
	cfgPath  string
	dbPath   string
	logPath  string
	logs     *os.File
	base     string
	fake     *fakellm.Server
	upstream *httptest.Server
	proc     *exec.Cmd
	w        *world
}

func newIssue72UnknownEnv(t *testing.T) *issue72UnknownEnv {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "proxy")
	build := exec.Command("go", "build", "-o", bin, "./cmd/proxy")
	build.Dir = repoRoot()
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build proxy: %v\n%s", err, out)
	}
	fake := fakellm.New(fakellm.Options{Name: "issue72-unknown-hour"})
	upstream := httptest.NewServer(fake)
	t.Cleanup(upstream.Close)
	port, err := freePort()
	if err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dir, "proxy.db")
	cfg := map[string]any{
		"server":        map[string]any{"bind": fmt.Sprintf("127.0.0.1:%d", port), "request_timeout_seconds": 30},
		"data_store":    map[string]any{"db_path": dbPath},
		"admin_session": map[string]any{"cookie_name": "admin_sid", "secret": "issue72-unknown-secret", "ttl_seconds": 3600},
		"logging":       map[string]any{"level": "warn", "access_log": filepath.Join(dir, "access.log"), "error_log": filepath.Join(dir, "error.log")},
	}
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, mustJSON(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "proxy.out")
	logs, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	e := &issue72UnknownEnv{
		t: t, dir: dir, bin: bin, cfgPath: cfgPath, dbPath: dbPath, logPath: logPath,
		logs: logs, base: fmt.Sprintf("http://127.0.0.1:%d", port), fake: fake, upstream: upstream,
	}
	t.Cleanup(func() {
		e.stop()
		_ = logs.Close()
	})
	return e
}

func (e *issue72UnknownEnv) logText() string {
	raw, _ := os.ReadFile(e.logPath)
	return string(raw)
}

func (e *issue72UnknownEnv) start() {
	e.t.Helper()
	e.proc = exec.Command(e.bin, "-config", e.cfgPath)
	e.proc.Dir = e.dir
	e.proc.Env = append(os.Environ(), "ADMIN_PASSWORD="+adminPassword, "LOG_LEVEL=warn",
		"UPSTREAM_ERROR_LOG_PATH="+filepath.Join(e.dir, "upstream-error.log"), "TRACE_STORE_ENABLED=false")
	e.proc.Stdout, e.proc.Stderr = e.logs, e.logs
	if err := e.proc.Start(); err != nil {
		e.t.Fatal(err)
	}
	if err := waitHealthy(e.base, 30*time.Second); err != nil {
		e.t.Fatalf("%v: %s", err, e.logText())
	}
	jar, _ := cookiejar.New(nil)
	admin := &http.Client{Jar: jar, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := admin.PostForm(e.base+"/login", url.Values{"username": {"admin"}, "password": {adminPassword}})
	if err != nil {
		e.t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		e.t.Fatalf("admin login: HTTP %d; %s", resp.StatusCode, e.logText())
	}
	e.w = &world{base: e.base, admin: admin}
}

func (e *issue72UnknownEnv) stop() {
	if e.proc == nil {
		return
	}
	proc := e.proc
	e.proc = nil
	_ = proc.Process.Signal(os.Interrupt)
	done := make(chan error, 1)
	go func() { done <- proc.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			e.t.Errorf("proxy shutdown: %v; %s", err, e.logText())
		}
	case <-time.After(10 * time.Second):
		_ = proc.Process.Kill()
		<-done
		e.t.Errorf("proxy shutdown timed out; %s", e.logText())
	}
	e.w = nil
}

func (e *issue72UnknownEnv) admin(method, path string, body any, want int) []byte {
	e.t.Helper()
	code, raw := e.w.adminDo(method, path, body)
	if code != want {
		e.t.Fatalf("%s %s: HTTP %d want %d: %s; proxy log:\n%s", method, path, code, want, raw, e.logText())
	}
	return raw
}

func (e *issue72UnknownEnv) configure(clients []string) {
	e.t.Helper()
	e.admin("POST", "/admin/data/job-levels", map[string]any{"name": "level", "coefficient_daily": 1}, 201)
	e.admin("POST", "/admin/data/targets", map[string]any{
		"name": "target", "endpoint_type": "openai", "endpoint": e.upstream.URL + "/openai/v1",
		"api_key": "fake-key", "usage_adapter": "standard",
		"model_mappings": []any{map[string]any{"upstream": issue72UnknownModel, "fallback": "issue72"}},
	}, 201)
	e.admin("POST", "/admin/data/pricing-templates", map[string]any{
		"target": "target", "model": issue72UnknownModel, "mode": "flat",
		"base_rates": usage.CostRates{InputPer1MTokens: 2},
	}, 201)
	e.admin("POST", "/admin/data/model-groups", map[string]any{
		"name": "models", "daily_usd": 100, "cost_coefficient": 1,
		"bindings": []any{map[string]any{"target": "target", "model": issue72UnknownModel}},
	}, 201)
	for _, name := range clients {
		e.admin("POST", "/admin/data/clients", map[string]any{
			"name": name, "access_key": "sk-issue72-unknown-" + name, "job_level": "level",
			"allowed_targets": []string{"target"},
		}, 201)
		e.admin("PUT", "/admin/data/quota/"+name, map[string]any{
			"job_level": "level", "quota_groups": []any{map[string]any{"name": "models"}},
		}, 200)
	}
}

func (e *issue72UnknownEnv) call(clientName string) (int, string) {
	e.t.Helper()
	body := `{"model":"issue72","messages":[{"role":"user","content":"[[fake usage.in=1000000 usage.out=0 usage.cached=0 usage.reasoning=0]]"}]}`
	req, err := http.NewRequest("POST", e.base+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		e.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer sk-issue72-unknown-"+clientName)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		e.t.Fatal(err)
	}
	return resp.StatusCode, string(raw)
}

func (e *issue72UnknownEnv) seed(clientName string) {
	e.t.Helper()
	if code, raw := e.call(clientName); code != 200 {
		e.t.Fatalf("seed %s: %d %s", clientName, code, raw)
	}
}

func (e *issue72UnknownEnv) readEvents(clientName string) []usage.Event {
	e.t.Helper()
	var result struct {
		Events []usage.Event `json:"events"`
	}
	if err := json.Unmarshal(e.admin("GET", "/admin/data/usage/events?client_name="+url.QueryEscape(clientName), nil, 200), &result); err != nil {
		e.t.Fatal(err)
	}
	return result.Events
}

// waitSingleEvent waits until the client has exactly the seeded ledger row.
func (e *issue72UnknownEnv) waitSingleEvent(clientName string) usage.Event {
	e.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		events := e.readEvents(clientName)
		if len(events) == 1 {
			return events[0]
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("%s: %d ledger events, want 1", clientName, len(events))
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (e *issue72UnknownEnv) aggregate(clientName string, from, to *time.Time, want int) []byte {
	e.t.Helper()
	q := url.Values{"group_by": {"hour"}, "client_name": {clientName}}
	if from != nil {
		q.Set("from", from.UTC().Format(time.RFC3339))
	}
	if to != nil {
		q.Set("to", to.UTC().Format(time.RFC3339))
	}
	raw := e.admin("GET", "/admin/data/usage/aggregate?"+q.Encode(), nil, want)
	if want >= 400 {
		e.t.Logf("%s aggregate %v..%v: HTTP %d %s", clientName, from, to, want, raw)
	}
	return raw
}

func (e *issue72UnknownEnv) assertAggregate(clientName string, requests, input int64, cost float64) {
	e.t.Helper()
	var result usage.AggregateResult
	if err := json.Unmarshal(e.aggregate(clientName, nil, nil, 200), &result); err != nil {
		e.t.Fatal(err)
	}
	if result.Totals.Requests != requests || result.Totals.InputTokens != input || math.Abs(result.Totals.EstimatedCost-cost) > 1e-9 {
		e.t.Fatalf("%s aggregate: %+v, want requests=%d input=%d cost=%v", clientName, result.Totals, requests, input, cost)
	}
}

func (e *issue72UnknownEnv) assertWindowEmpty(clientName string, from, to time.Time) {
	e.t.Helper()
	var result usage.AggregateResult
	if err := json.Unmarshal(e.aggregate(clientName, &from, &to, 200), &result); err != nil {
		e.t.Fatal(err)
	}
	if result.Totals.Requests != 0 || result.Totals.InputTokens != 0 || math.Abs(result.Totals.EstimatedCost) > 1e-9 {
		e.t.Fatalf("%s window %s..%s is not empty: %+v", clientName, from, to, result.Totals)
	}
}

func (e *issue72UnknownEnv) quotaStatus(clientName string) map[string]any {
	e.t.Helper()
	return decode(e.t, e.admin("GET", "/admin/data/quota/"+url.PathEscape(clientName), nil, 200))
}

func (e *issue72UnknownEnv) waitQuotaHealthy(clientName string, used float64) {
	e.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		status := e.quotaStatus(clientName)
		if dig(status, "calibration_error") == nil && dig(status, "groups/models/quotas/daily/used") == used {
			return
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("%s quota not healthy at used=%v: %+v", clientName, used, status)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (e *issue72UnknownEnv) waitQuotaCalibrationError(clientName string) {
	e.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var status map[string]any
	for {
		status = e.quotaStatus(clientName)
		if msg, _ := dig(status, "calibration_error").(string); msg != "" {
			e.t.Logf("%s calibration_error: %s", clientName, msg)
			return
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("%s quota has no calibration_error: %+v", clientName, status)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// issue72UnknownFixture records the exact bytes written while the proxy was
// stopped and the pre-injection state of the cells that identity would share.
type issue72UnknownFixture struct {
	rawKey       []byte
	rawValue     []byte
	originKey    []byte
	originValue  []byte
	quotaKey     []byte
	quotaCell    []byte
	quotaProof   []byte
	usageAggKey  []byte
	usageAggCell []byte
}

func issue72HourKey(t time.Time) string {
	return t.UTC().Truncate(time.Hour).Format("2006-01-02T15")
}

// issue72QuotaKey mirrors quotaAggKey's shape for names without escaped
// separators: hour|client|lower(target)|lower(model).
func issue72QuotaKey(evt usage.Event) []byte {
	return []byte(issue72HourKey(evt.Timestamp) + "|" + strings.TrimSpace(evt.ClientName) + "|" +
		strings.ToLower(strings.TrimSpace(evt.Target)) + "|" + strings.ToLower(strings.TrimSpace(evt.Model)))
}

// issue72UsageAggKey mirrors aggKey: v2|hour|hex(endpoint)|hex(client)|hex(model)|hex(target).
func issue72UsageAggKey(evt usage.Event) []byte {
	part := func(s string) string { return hex.EncodeToString([]byte(s)) }
	return []byte("v2|" + issue72HourKey(evt.Timestamp) + "|" +
		part(strings.ToLower(strings.TrimSpace(evt.EndpointType))) + "|" +
		part(strings.TrimSpace(evt.ClientName)) + "|" +
		part(strings.ToLower(strings.TrimSpace(evt.Model))) + "|" +
		part(strings.ToLower(strings.TrimSpace(evt.Target))))
}

func issue72RawKey(evt usage.Event) []byte {
	return []byte(evt.Timestamp.UTC().Format(time.RFC3339Nano) + "_" + evt.RecordID)
}

// inject copies a real, fully attested event to a new identity and writes it
// directly into usage_events, without any receipt, owner, index entry, hourly
// cell or quota cell. The seeded identity is verified to be complete first, so
// the later failure can only come from the injected unknown row.
func (e *issue72UnknownEnv) inject(event usage.Event, at time.Time, recordID string) issue72UnknownFixture {
	e.t.Helper()
	copied := event
	copied.RecordID = recordID
	copied.Timestamp = at.UTC()
	fx := issue72UnknownFixture{
		rawKey:      issue72RawKey(copied),
		rawValue:    mustJSON(copied),
		originKey:   issue72RawKey(event),
		quotaKey:    issue72QuotaKey(copied),
		usageAggKey: issue72UsageAggKey(copied),
	}
	originQuotaKey := issue72QuotaKey(event)
	originUsageAggKey := issue72UsageAggKey(event)
	db, err := bolt.Open(e.dbPath, 0o600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		e.t.Fatal(err)
	}
	err = db.Update(func(tx *bolt.Tx) error {
		raw := tx.Bucket([]byte("usage_events"))
		receipts := tx.Bucket([]byte("usage_record_receipts_v1"))
		owners := tx.Bucket([]byte("usage_raw_owners_v1"))
		index := tx.Bucket([]byte("usage_events_by_client_v1"))
		quota := tx.Bucket([]byte("quota_usage_agg_hourly"))
		agg := tx.Bucket([]byte("usage_agg_hourly"))
		if raw == nil || receipts == nil || owners == nil || index == nil || quota == nil || agg == nil {
			var missing []string
			for name, bucket := range map[string]*bolt.Bucket{
				"usage_events": raw, "usage_record_receipts_v1": receipts, "usage_raw_owners_v1": owners,
				"usage_events_by_client_v1": index, "quota_usage_agg_hourly": quota,
				"usage_agg_hourly": agg,
			} {
				if bucket == nil {
					missing = append(missing, name)
				}
			}
			return fmt.Errorf("expected buckets are missing: %v", missing)
		}
		origin := raw.Get(fx.originKey)
		if len(origin) == 0 {
			return fmt.Errorf("seeded raw row %q missing", fx.originKey)
		}
		fx.originValue = bytes.Clone(origin)
		// A live identity in a tracked database is attested by its raw owner,
		// client index entry and both aggregation cells. (Prune receipts and
		// fresh attestations only appear for older or untracked ledgers.)
		if owners.Get(fx.originKey) == nil {
			return fmt.Errorf("seeded raw owner missing")
		}
		indexed := false
		if err := index.ForEach(func(_, v []byte) error {
			if bytes.Equal(v, fx.originKey) {
				indexed = true
			}
			return nil
		}); err != nil {
			return err
		}
		if !indexed {
			return fmt.Errorf("seeded raw row missing from the client index")
		}
		if quota.Get(originQuotaKey) == nil {
			return fmt.Errorf("seeded quota cell missing")
		}
		if agg.Get(originUsageAggKey) == nil {
			return fmt.Errorf("seeded hourly usage cell missing")
		}
		if receipts.Get(fx.rawKey) != nil || owners.Get(fx.rawKey) != nil {
			return fmt.Errorf("injected identity is already attested")
		}
		fx.quotaCell = bytes.Clone(quota.Get(fx.quotaKey))
		if proofs := tx.Bucket([]byte("usage_rollup_proofs_v2")); proofs != nil {
			fx.quotaProof = bytes.Clone(proofs.Get(append(append([]byte("quota_usage_agg_hourly"), 0), fx.quotaKey...)))
		}
		fx.usageAggCell = bytes.Clone(agg.Get(fx.usageAggKey))
		return raw.Put(fx.rawKey, fx.rawValue)
	})
	closeErr := db.Close()
	if err != nil || closeErr != nil {
		e.t.Fatalf("inject unknown usage row: %v / %v", err, closeErr)
	}
	return fx
}

// verifyInjected proves the proxy never deleted, rewrote or "repaired" the
// unknown row or synthesized receipts, owners, index entries or aggregation
// for it across restarts.
func (e *issue72UnknownEnv) verifyInjected(fx issue72UnknownFixture) {
	e.t.Helper()
	db, err := bolt.Open(e.dbPath, 0o600, &bolt.Options{ReadOnly: true, Timeout: time.Second})
	if err != nil {
		e.t.Fatal(err)
	}
	err = db.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket([]byte("usage_events"))
		if raw == nil {
			return fmt.Errorf("usage_events bucket missing")
		}
		if got := raw.Get(fx.rawKey); !bytes.Equal(got, fx.rawValue) {
			return fmt.Errorf("injected raw row changed: got %s want %s", got, fx.rawValue)
		}
		if got := raw.Get(fx.originKey); !bytes.Equal(got, fx.originValue) {
			return fmt.Errorf("original raw row changed")
		}
		for _, name := range []string{
			"usage_record_receipts_v1", "usage_raw_owners_v1",
			"usage_receipt_owner_hashes_v1", "usage_fresh_commit_attestations_v1",
		} {
			b := tx.Bucket([]byte(name))
			if b == nil {
				continue
			}
			if b.Get(fx.rawKey) != nil {
				return fmt.Errorf("injected identity gained a %s entry", name)
			}
		}
		index := tx.Bucket([]byte("usage_events_by_client_v1"))
		if index == nil {
			return fmt.Errorf("client index bucket missing")
		}
		if err := index.ForEach(func(_, v []byte) error {
			if bytes.Equal(v, fx.rawKey) {
				return fmt.Errorf("injected identity entered the client index")
			}
			return nil
		}); err != nil {
			return err
		}
		quota := tx.Bucket([]byte("quota_usage_agg_hourly"))
		if quota == nil {
			return fmt.Errorf("quota aggregation bucket missing")
		}
		if got := quota.Get(fx.quotaKey); !bytes.Equal(got, fx.quotaCell) {
			return fmt.Errorf("injected quota cell changed: got %s want %s", got, fx.quotaCell)
		}
		var proof []byte
		if proofs := tx.Bucket([]byte("usage_rollup_proofs_v2")); proofs != nil {
			proof = proofs.Get(append(append([]byte("quota_usage_agg_hourly"), 0), fx.quotaKey...))
		}
		if !bytes.Equal(proof, fx.quotaProof) {
			return fmt.Errorf("injected quota proof changed: got %s want %s", proof, fx.quotaProof)
		}
		agg := tx.Bucket([]byte("usage_agg_hourly"))
		if agg == nil {
			return fmt.Errorf("hourly usage aggregation bucket missing")
		}
		if got := agg.Get(fx.usageAggKey); !bytes.Equal(got, fx.usageAggCell) {
			return fmt.Errorf("injected hourly usage cell changed: got %s want %s", got, fx.usageAggCell)
		}
		return nil
	})
	if err != nil {
		e.t.Fatalf("verify injected ledger after restart: %v", err)
	}
	if err := db.Close(); err != nil {
		e.t.Fatal(err)
	}
}

func runIssue72UnknownHourScenario(t *testing.T, future bool) {
	env := newIssue72UnknownEnv(t)
	env.start()
	env.configure([]string{"alice", "bob"})
	env.seed("alice")
	env.seed("bob")

	aliceEvent := env.waitSingleEvent("alice")
	bobEvent := env.waitSingleEvent("bob")
	if aliceEvent.RecordID == "" || aliceEvent.ClientName != "alice" || aliceEvent.InputTokens != 1000000 {
		t.Fatalf("unexpected alice event: %+v", aliceEvent)
	}
	if bobEvent.RecordID == "" || bobEvent.ClientName != "bob" || bobEvent.InputTokens != 1000000 {
		t.Fatalf("unexpected bob event: %+v", bobEvent)
	}
	// Baseline: one attested request per client, cost 1M input tokens * $2/M.
	env.assertAggregate("alice", 1, 1000000, 2)
	env.assertAggregate("bob", 1, 1000000, 2)
	env.waitQuotaHealthy("alice", 2)
	env.waitQuotaHealthy("bob", 2)

	injectedAt := time.Now().UTC().Truncate(time.Hour)
	if future {
		injectedAt = time.Now().UTC().Add(48 * time.Hour)
	}
	env.stop()
	fx := env.inject(aliceEvent, injectedAt, "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaa00e2")
	futureHour := injectedAt.UTC().Truncate(time.Hour)
	futureEnd := futureHour.Add(time.Hour)

	for restart := 0; restart < 2; restart++ {
		env.start()
		if future {
			// The injected row is outside today's aggregate and calibration
			// windows: it must neither be blocked as unrelated evidence nor be
			// counted as new usage (a synthesized row would add 1 request and $2
			// per restart).
			env.assertAggregate("alice", int64(1+restart), int64(1000000*(1+restart)), 2*float64(1+restart))
			env.waitQuotaHealthy("alice", 2*float64(1+restart))
			// The window that contains the unknown row must fail closed rather
			// than report a complete total of zero.
			env.aggregate("alice", &futureHour, &futureEnd, 500)
			env.assertWindowEmpty("bob", futureHour, futureEnd)
			env.assertAggregate("bob", int64(1+restart), int64(1000000*(1+restart)), 2*float64(1+restart))
			env.waitQuotaHealthy("bob", 2*float64(1+restart))
			seq := env.fake.LastSeq()
			if code, raw := env.call("alice"); code != 200 || env.fake.LastSeq() == seq {
				t.Fatalf("future-hour evidence blocked today's admission: %d %s", code, raw)
			}
			seq = env.fake.LastSeq()
			if code, raw := env.call("bob"); code != 200 || env.fake.LastSeq() == seq {
				t.Fatalf("unrelated admission: %d %s", code, raw)
			}
		} else {
			// The row is inside the default aggregate and quota calibration
			// windows: every read and admission must fail closed instead of
			// returning the attested total or 2 as if the row were zero.
			env.aggregate("alice", nil, nil, 500)
			env.waitQuotaCalibrationError("alice")
			env.assertAggregate("bob", int64(1+restart), int64(1000000*(1+restart)), 2*float64(1+restart))
			env.waitQuotaHealthy("bob", 2*float64(1+restart))
			seq := env.fake.LastSeq()
			if code, raw := env.call("alice"); code != 503 || !strings.Contains(raw, "accounting_unavailable") {
				t.Fatalf("affected admission: %d %s", code, raw)
			}
			if env.fake.LastSeq() != seq {
				t.Fatal("affected request reached fakellm")
			}
			if code, raw := env.call("bob"); code != 200 || env.fake.LastSeq() == seq {
				t.Fatalf("unrelated admission: %d %s", code, raw)
			}
		}
		env.stop()
		env.verifyInjected(fx)
	}
}
