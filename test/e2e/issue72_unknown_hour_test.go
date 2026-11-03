//go:build e2e

package e2e

// Issue #72 requires usage identities that were never attested to fail closed:
// a raw ledger row without its identity evidence must not be treated as zero,
// as complete, or as newly valid usage. The sidecar test injects rejected
// historical evidence; this test owns a separate proxy and database and
// injects an *unknown* raw row directly into usage_events while the proxy is
// stopped. The injected row has no raw owner record, no client index entry and
// no hourly usage/quota aggregation cell -- those are exactly the identity
// evidence a live row carries in a tracked database. Receipts and fresh
// attestations are not written for live rows in a fresh tracked database
// either (they only exist for pruned or untracked ledgers), so their absence is
// not what makes this row unknown.
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
//
// Both scenarios are deterministic at any wall-clock time. Quota daily
// counters reset at the UTC day boundary, so healthy expectations are derived
// from the ledger rows inside the current UTC day on every poll rather than
// compared against a fixed cumulative total. The current-hour scenario also
// injects a sentinel unknown row exactly at the next UTC midnight: before
// midnight it is outside every calibration window, while after midnight it
// stays inside the window whose start moved to midnight. This keeps the
// fail-closed premise when a run crosses midnight, including the rare midnight
// that starts both an ISO week and a month and would otherwise move the
// earliest quota period start past the current-hour row.

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
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/fakellm"
	"github.com/LeXwDeX/LLM-Proxy/internal/quota"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	bolt "go.etcd.io/bbolt"
)

const (
	issue72UnknownModel = "Issue72Model"
	// issue72InputPricePer1M is the flat input price configured below; every
	// seeded request uses exactly 1M input tokens, so one request costs $2.
	issue72InputPricePer1M = 2.0
	// Record IDs of the raw rows this test writes directly into usage_events.
	// They are not returned by the client index and carry no identity evidence.
	issue72CurrentRecordID  = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaa00e2"
	issue72MidnightRecordID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaa00e3"
	// issue72BoundaryGrace only needs to exceed the gap between an event's
	// durable timestamp and the quota increment's later clock read (the row is
	// committed first). A row inside it may legitimately belong to either side
	// of the UTC day boundary, so both daily totals are accepted.
	issue72BoundaryGrace = time.Minute
)

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

func (e *issue72UnknownEnv) errorLogText() string {
	raw, _ := os.ReadFile(filepath.Join(e.dir, "error.log"))
	return string(raw)
}

// waitErrorLogContains waits until one single log line after offset contains
// every needle (not one needle per line) and returns the new log length. The
// admin handler logs the error before writing the response, so this is direct
// attribution evidence for a 500 rather than a heuristic. Offsets keep evidence
// from an earlier restart from satisfying a later assertion.
func (e *issue72UnknownEnv) waitErrorLogContains(offset int, needles ...string) int {
	e.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		text := e.errorLogText()
		if len(text) >= offset {
			for _, line := range strings.Split(text[offset:], "\n") {
				matched := true
				for _, needle := range needles {
					if !strings.Contains(line, needle) {
						matched = false
						break
					}
				}
				if matched {
					return len(text)
				}
			}
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("error log has no line containing %q; tail:\n%s", needles, tailOf(text, offset))
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func tailOf(text string, offset int) string {
	if offset >= 0 && offset <= len(text) {
		return text[offset:]
	}
	return text
}

func (e *issue72UnknownEnv) start() {
	e.t.Helper()
	e.proc = exec.Command(e.bin, "-config", e.cfgPath)
	e.proc.Dir = e.dir
	e.proc.Env = append(os.Environ(), "ADMIN_PASSWORD="+adminPassword, "LOG_LEVEL=warn",
		"UPSTREAM_ERROR_LOG_PATH="+filepath.Join(e.dir, "upstream-error.log"), "TRACE_STORE_ENABLED=false",
		"SERVER_BIND="+strings.TrimPrefix(e.base, "http://"))
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
	if e.proc == nil || e.proc.Process == nil {
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
		"base_rates": usage.CostRates{InputPer1MTokens: issue72InputPricePer1M},
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

// issue72EventCost applies the proxy's own cost formula to one ledger row
// using the flat input price configured for this scenario.
func issue72EventCost(ev usage.Event) float64 {
	return usage.CostRates{InputPer1MTokens: issue72InputPricePer1M}.CostFor(
		ev.InputTokens, ev.OutputTokens, ev.CachedTokens, ev.CacheCreationTokens)
}

// issue72RealEvents drops the unknown rows this test injected directly into the
// ledger. They are exactly the rows the proxy must never treat as usage.
func issue72RealEvents(events []usage.Event) []usage.Event {
	real := make([]usage.Event, 0, len(events))
	for _, ev := range events {
		if ev.RecordID == issue72CurrentRecordID || ev.RecordID == issue72MidnightRecordID {
			continue
		}
		real = append(real, ev)
	}
	return real
}

// issue72DailyUsedCandidates recomputes the quota daily counter implied by the
// known-good rows at now. The daily window starts at 00:00 UTC, so a fixed
// cumulative expectation would go stale if the run crosses a UTC day or month
// boundary; callers evaluate this on every poll instead. A row timestamped just
// before the day start may still have been charged to either period, because
// the durable row is committed before the in-memory increment reads the clock;
// such rows contribute to several candidate totals. Any other extra or missing
// dollar still fails the assertion.
func issue72DailyUsedCandidates(t *testing.T, events []usage.Event, now time.Time) []float64 {
	t.Helper()
	dayStart, _, err := quota.PeriodRange(quota.DimensionDaily, now)
	if err != nil {
		t.Fatalf("daily quota period: %v", err)
	}
	base := 0.0
	var straddling []float64
	for _, ev := range events {
		at := ev.Timestamp.UTC()
		cost := issue72EventCost(ev)
		switch {
		case !at.Before(dayStart):
			base += cost
		case !at.Before(dayStart.Add(-issue72BoundaryGrace)):
			straddling = append(straddling, cost)
		}
	}
	candidates := []float64{base}
	for _, cost := range straddling {
		n := len(candidates)
		for i := 0; i < n; i++ {
			candidates = append(candidates, candidates[i]+cost)
		}
	}
	// Subset sums repeat when the base is zero; callers compare exact values.
	unique := make([]float64, 0, len(candidates))
	for _, candidate := range candidates {
		duplicate := false
		for _, kept := range unique {
			if math.Abs(kept-candidate) <= 1e-9 {
				duplicate = true
				break
			}
		}
		if !duplicate {
			unique = append(unique, candidate)
		}
	}
	return unique
}

// waitQuotaHealthy waits until calibration is healthy and the daily counter
// matches the usage implied by the requests this test issued in the current UTC
// day. requests is the exact number of known-good ledger rows expected for the
// client, so a missing or synthesized row still fails rather than being folded
// into the expectation. Both the expectation and the day window are recomputed
// on every poll, which makes a restart or a boundary crossing deterministic.
func (e *issue72UnknownEnv) waitQuotaHealthy(clientName string, requests int) {
	e.t.Helper()
	// Poll past calibrationRequestTimeout (15s) so a slow machine's calibration
	// cannot fail a run that would become healthy.
	deadline := time.Now().Add(45 * time.Second)
	var status map[string]any
	known := 0
	for {
		events := issue72RealEvents(e.readEvents(clientName))
		known = len(events)
		if known == requests {
			candidates := issue72DailyUsedCandidates(e.t, events, time.Now().UTC())
			status = e.quotaStatus(clientName)
			if dig(status, "calibration_error") == nil {
				if used, ok := dig(status, "groups/models/quotas/daily/used").(float64); ok {
					for _, want := range candidates {
						if math.Abs(used-want) <= 1e-9 {
							return
						}
					}
				}
			}
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("%s quota not healthy: %d known request(s), want %d, status=%+v", clientName, known, requests, status)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (e *issue72UnknownEnv) waitQuotaCalibrationError(clientName string) {
	e.t.Helper()
	// calibrationRequestTimeout is 15s and a request-path timeout writes its
	// own calibration_error ("calibration timed out..."). Poll well past that
	// timeout and require the incomplete-history prefix so a timeout can never
	// satisfy the assertion.
	const integrityPrefix = "usage history is incomplete"
	deadline := time.Now().Add(45 * time.Second)
	var msg string
	for {
		status := e.quotaStatus(clientName)
		msg, _ = dig(status, "calibration_error").(string)
		if strings.Contains(msg, integrityPrefix) {
			e.t.Logf("%s calibration_error: %s", clientName, msg)
			return
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("%s quota calibration_error %q does not contain %q: %+v", clientName, msg, integrityPrefix, status)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// issue72UnknownFixture records the exact bytes written while the proxy was
// stopped and the pre-injection state of the derived objects the injected
// identity would share. The raw row itself is the only thing written for the
// unknown identity; every other object below must stay byte-identical (or
// absent) across restarts.
type issue72UnknownFixture struct {
	rawKey          []byte
	rawValue        []byte
	originKey       []byte
	originValue     []byte
	quotaKey        []byte
	quotaCell       []byte
	quotaProof      []byte
	usageAggKey     []byte
	usageAggCell    []byte
	usageProof      []byte
	clientHourKey   []byte
	clientHourCell  []byte
	quarantineValue []byte
	quarantineCount []byte
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

// issue72ClientHourKey mirrors clientHourKey: hex(client)|hour.
func issue72ClientHourKey(evt usage.Event) []byte {
	return []byte(hex.EncodeToString([]byte(strings.TrimSpace(evt.ClientName))) + "|" +
		evt.Timestamp.UTC().Truncate(time.Hour).Format("2006-01-02T15"))
}

// issue72ProofKey mirrors rollupProofKey: source + NUL + cell key.
func issue72ProofKey(source string, key []byte) []byte {
	out := append([]byte(source), 0)
	return append(out, key...)
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
		clientHours := tx.Bucket([]byte("usage_client_hours_v1"))
		quarantine := tx.Bucket([]byte("usage_events_quarantine_v1"))
		meta := tx.Bucket([]byte("meta"))
		quota := tx.Bucket([]byte("quota_usage_agg_hourly"))
		agg := tx.Bucket([]byte("usage_agg_hourly"))
		if raw == nil || receipts == nil || owners == nil || index == nil || clientHours == nil || quarantine == nil || meta == nil || quota == nil || agg == nil {
			var missing []string
			for name, bucket := range map[string]*bolt.Bucket{
				"usage_events": raw, "usage_record_receipts_v1": receipts, "usage_raw_owners_v1": owners,
				"usage_events_by_client_v1": index, "usage_client_hours_v1": clientHours,
				"usage_events_quarantine_v1": quarantine, "meta": meta,
				"quota_usage_agg_hourly": quota, "usage_agg_hourly": agg,
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
		// client index entry, client-hour counter and both aggregation cells.
		// (Prune receipts and fresh attestations are not written for live rows
		// in a new tracked database; see the header comment.)
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
		if clientHours.Get(issue72ClientHourKey(event)) == nil {
			return fmt.Errorf("seeded client-hour counter missing")
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
		// Snapshot every derived object keyed by the injected identity. The
		// client-hour key equals the seeded one for the current-hour case and is
		// absent for the future-hour case; both are compared byte-for-byte.
		fx.clientHourKey = issue72ClientHourKey(copied)
		fx.clientHourCell = bytes.Clone(clientHours.Get(fx.clientHourKey))
		fx.quarantineValue = bytes.Clone(quarantine.Get(fx.rawKey))
		fx.quarantineCount = bytes.Clone(meta.Get([]byte("usage_events_quarantine_count_v1")))
		fx.quotaCell = bytes.Clone(quota.Get(fx.quotaKey))
		fx.usageAggCell = bytes.Clone(agg.Get(fx.usageAggKey))
		// The proof bucket is created lazily and does not exist in a fresh
		// tracked database until a proof is written, so snapshot both injected
		// proof cells nil-safely.
		if proofs := tx.Bucket([]byte("usage_rollup_proofs_v2")); proofs != nil {
			fx.quotaProof = bytes.Clone(proofs.Get(issue72ProofKey("quota_usage_agg_hourly", fx.quotaKey)))
			fx.usageProof = bytes.Clone(proofs.Get(issue72ProofKey("usage_agg_hourly", fx.usageAggKey)))
		}
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
		// Receipts are created with the schema and raw owners by the seeded live
		// writes (inject requires them too), so both buckets must exist; neither
		// may gain an entry for the injected identity.
		for _, name := range []string{"usage_record_receipts_v1", "usage_raw_owners_v1"} {
			b := tx.Bucket([]byte(name))
			if b == nil {
				return fmt.Errorf("%s bucket missing", name)
			}
			if b.Get(fx.rawKey) != nil {
				return fmt.Errorf("injected identity gained a %s entry", name)
			}
		}
		// Receipt-owner hashes and fresh attestations are only written for pruned
		// or untracked ledgers, so a fresh tracked database may never create these
		// buckets; only assert that no entry for the injected identity appeared,
		// and do not treat nil==nil as coverage. The client-hour, client-index,
		// quota-cell and usage-cell checks below provide that coverage.
		for _, name := range []string{"usage_receipt_owner_hashes_v1", "usage_fresh_commit_attestations_v1"} {
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
		clientHours := tx.Bucket([]byte("usage_client_hours_v1"))
		if clientHours == nil {
			return fmt.Errorf("client hours bucket missing")
		}
		if got := clientHours.Get(fx.clientHourKey); !bytes.Equal(got, fx.clientHourCell) {
			return fmt.Errorf("injected client-hour counter changed: got %s want %s", got, fx.clientHourCell)
		}
		quarantine := tx.Bucket([]byte("usage_events_quarantine_v1"))
		if quarantine == nil {
			return fmt.Errorf("quarantine bucket missing")
		}
		if got := quarantine.Get(fx.rawKey); !bytes.Equal(got, fx.quarantineValue) {
			return fmt.Errorf("injected raw row was quarantined: got %s want %s", got, fx.quarantineValue)
		}
		meta := tx.Bucket([]byte("meta"))
		if meta == nil {
			return fmt.Errorf("meta bucket missing")
		}
		if got := meta.Get([]byte("usage_events_quarantine_count_v1")); !bytes.Equal(got, fx.quarantineCount) {
			return fmt.Errorf("quarantine count changed: got %s want %s", got, fx.quarantineCount)
		}
		quota := tx.Bucket([]byte("quota_usage_agg_hourly"))
		if quota == nil {
			return fmt.Errorf("quota aggregation bucket missing")
		}
		if got := quota.Get(fx.quotaKey); !bytes.Equal(got, fx.quotaCell) {
			return fmt.Errorf("injected quota cell changed: got %s want %s", got, fx.quotaCell)
		}
		// The proof bucket is lazily created and may be absent; when present, no
		// proof may have appeared for either cell of the injected identity.
		var quotaProof, usageProof []byte
		if proofs := tx.Bucket([]byte("usage_rollup_proofs_v2")); proofs != nil {
			quotaProof = proofs.Get(issue72ProofKey("quota_usage_agg_hourly", fx.quotaKey))
			usageProof = proofs.Get(issue72ProofKey("usage_agg_hourly", fx.usageAggKey))
		}
		if !bytes.Equal(quotaProof, fx.quotaProof) {
			return fmt.Errorf("injected quota proof changed: got %s want %s", quotaProof, fx.quotaProof)
		}
		if !bytes.Equal(usageProof, fx.usageProof) {
			return fmt.Errorf("injected usage proof changed: got %s want %s", usageProof, fx.usageProof)
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

// issue72NextUTCMidnight returns the first 00:00 UTC strictly after now.
func issue72NextUTCMidnight(now time.Time) time.Time {
	now = now.UTC()
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, 1)
}

func runIssue72UnknownHourScenario(t *testing.T, future bool) {
	t.Helper()
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
	env.waitQuotaHealthy("alice", 1)
	env.waitQuotaHealthy("bob", 1)

	now := time.Now().UTC()
	injectedAt := now.Truncate(time.Hour)
	nextMidnight := issue72NextUTCMidnight(now)
	if future {
		injectedAt = now.Add(48 * time.Hour)
	}
	env.stop()
	fixtures := []issue72UnknownFixture{
		env.inject(aliceEvent, injectedAt, issue72CurrentRecordID),
	}
	if !future {
		// Sentinel unknown row exactly at the upcoming UTC midnight. Before
		// midnight it is outside every calibration window; afterwards it stays
		// inside the window whose start just moved to midnight, even when that
		// midnight also starts an ISO week and a month and the current-hour row
		// would fall out of the earliest period start.
		fixtures = append(fixtures, env.inject(aliceEvent, nextMidnight, issue72MidnightRecordID))
	}
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
			env.waitQuotaHealthy("alice", 1+restart)
			// The window that contains the unknown row must fail closed rather
			// than report a complete total of zero. Bind the 500 to the admin
			// handler's own error log so a different internal failure cannot
			// satisfy this assertion.
			logOffset := len(env.errorLogText())
			env.aggregate("alice", &futureHour, &futureEnd, 500)
			env.waitErrorLogContains(logOffset, "failed to aggregate usage", "usage history is incomplete for exact pricing")
			env.assertWindowEmpty("bob", futureHour, futureEnd)
			env.assertAggregate("bob", int64(1+restart), int64(1000000*(1+restart)), 2*float64(1+restart))
			env.waitQuotaHealthy("bob", 1+restart)
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
			// returning the attested total as if the row were zero. The 500
			// must be the incomplete-history error, not any other internal fault.
			logOffset := len(env.errorLogText())
			env.aggregate("alice", nil, nil, 500)
			env.waitErrorLogContains(logOffset, "failed to aggregate usage", "usage history is incomplete for exact pricing")
			env.waitQuotaCalibrationError("alice")
			env.assertAggregate("bob", int64(1+restart), int64(1000000*(1+restart)), 2*float64(1+restart))
			env.waitQuotaHealthy("bob", 1+restart)
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
		for _, fx := range fixtures {
			env.verifyInjected(fx)
		}
	}
}

// issue72EarliestQuotaWindowStart mirrors quota.quotaPeriodStarts: a
// calibration reads the daily, ISO-week and monthly dimensions from one union
// window, so the earliest start decides whether a raw row is in scope.
func issue72EarliestQuotaWindowStart(t *testing.T, now time.Time) time.Time {
	t.Helper()
	var earliest time.Time
	for _, dim := range []string{quota.DimensionDaily, quota.DimensionWeekly, quota.DimensionMonthly} {
		from, _, err := quota.PeriodRange(dim, now)
		if err != nil {
			t.Fatalf("%s quota period: %v", dim, err)
		}
		if earliest.IsZero() || from.Before(earliest) {
			earliest = from
		}
	}
	return earliest
}

// TestIssue72UnknownHourBoundaryWindows locks the wall-clock premises of the
// scenario without starting a proxy. It proves that the daily quota expectation
// follows the current UTC day instead of a stale cumulative total, and that the
// current-hour row plus the next-midnight sentinel leave an unknown row inside
// the quota calibration window for observation times across the boundary --
// including the Monday month start where the current-hour row alone falls out.
func TestIssue72UnknownHourBoundaryWindows(t *testing.T) {
	const cost = issue72InputPricePer1M // one request uses exactly 1M input tokens
	eventAt := func(at time.Time) usage.Event {
		return usage.Event{Timestamp: at, InputTokens: 1000000}
	}
	// 2026-06-01 is both a Monday and a month start, the only kind of midnight
	// that moves every quota period start to the new day.
	dayStart := time.Date(2026, time.June, 1, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		events []usage.Event
		want   []float64
	}{
		{"before_boundary_outside_grace", []usage.Event{eventAt(dayStart.Add(-2 * time.Minute))}, []float64{0}},
		{"straddling_one", []usage.Event{eventAt(dayStart.Add(-30 * time.Second))}, []float64{0, cost}},
		{"straddling_two", []usage.Event{eventAt(dayStart.Add(-30 * time.Second)), eventAt(dayStart.Add(-10 * time.Second))}, []float64{0, cost, 2 * cost}},
		{"straddling_plus_current", []usage.Event{eventAt(dayStart.Add(-30 * time.Second)), eventAt(dayStart.Add(time.Second))}, []float64{cost, 2 * cost}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := issue72DailyUsedCandidates(t, tc.events, dayStart.Add(time.Hour))
			want := append([]float64(nil), tc.want...)
			sort.Float64s(got)
			sort.Float64s(want)
			if len(got) != len(want) {
				t.Fatalf("daily used candidates = %v, want %v", got, want)
			}
			for i := range want {
				if math.Abs(got[i]-want[i]) > 1e-9 {
					t.Fatalf("daily used candidates = %v, want %v", got, want)
				}
			}
		})
	}

	// Simulate a current-hour injection 30s before the Monday month start. The
	// current-hour row is inside the calibration window before midnight; after
	// midnight only the midnight sentinel remains in scope.
	injectNow := dayStart.Add(-30 * time.Second)
	injectedAt := injectNow.Truncate(time.Hour)
	sentinel := issue72NextUTCMidnight(injectNow)
	if !sentinel.Equal(dayStart) {
		t.Fatalf("next UTC midnight of %s = %s, want %s", injectNow, sentinel, dayStart)
	}
	for _, verify := range []time.Time{
		injectNow,
		dayStart.Add(-time.Second),
		dayStart,
		dayStart.Add(time.Second),
		dayStart.Add(time.Minute),
		dayStart.Add(12 * time.Hour),
	} {
		if verify.Before(injectNow) {
			continue
		}
		earliest := issue72EarliestQuotaWindowStart(t, verify)
		inside := func(at time.Time) bool {
			return !at.Before(earliest) && !at.After(verify)
		}
		current, midnight := inside(injectedAt), inside(sentinel)
		if !current && !midnight {
			t.Fatalf("verify %s: neither injected row is in quota window %s..%s (current %s, sentinel %s)",
				verify, earliest, verify, injectedAt, sentinel)
		}
		if verify.After(dayStart) && current {
			t.Fatalf("verify %s: Monday month start should move the window past the current-hour row %s (earliest %s)",
				verify, injectedAt, earliest)
		}
	}
}
