//go:build e2e

package e2e

// Issue #72 acceptance requires an isolated instance to lose already-committed
// raw evidence, restart, and still fail closed. This test deletes the raw
// usage_events row of a successful request while its raw owner, quota cell and
// usage cell stay intact, then restarts the real proxy twice. It verifies the
// damaged client is blocked (aggregate 500, calibration error, admission 503
// without reaching fakellm) while an unrelated client remains healthy.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	bolt "go.etcd.io/bbolt"
)

func TestIssue72HistoryEvidenceLoss(t *testing.T) {
	e := newIssue72UnknownEnv(t)
	e.start()
	e.configure([]string{"alice", "bob"})
	e.seed("alice")
	e.seed("bob")
	aliceEvent := e.waitSingleEvent("alice")
	e.waitSingleEvent("bob")
	e.stop()

	rawKey := issue72RawKey(aliceEvent)
	quotaKey := issue72QuotaKey(aliceEvent)
	db, err := bolt.Open(e.dbPath, 0o600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	err = db.Update(func(tx *bolt.Tx) error {
		raw := tx.Bucket([]byte("usage_events"))
		if raw == nil || raw.Get(rawKey) == nil {
			return fmt.Errorf("raw row %q missing before injection", rawKey)
		}
		owners := tx.Bucket([]byte("usage_raw_owners_v1"))
		if owners == nil || owners.Get(rawKey) == nil {
			return fmt.Errorf("raw owner %q missing before injection", rawKey)
		}
		quota := tx.Bucket([]byte("quota_usage_agg_hourly"))
		if quota == nil || quota.Get(quotaKey) == nil {
			return fmt.Errorf("quota cell %q missing before injection", quotaKey)
		}
		if err := raw.Delete(rawKey); err != nil {
			return err
		}
		return nil
	})
	closeErr := db.Close()
	if err != nil || closeErr != nil {
		t.Fatalf("inject historical evidence loss: %v / %v", err, closeErr)
	}

	for restart := 0; restart < 2; restart++ {
		e.start()
		e.admin("GET", "/admin/data/usage/aggregate?"+
			"group_by=hour&client_name=alice", nil, 500)
		e.waitQuotaCalibrationError("alice")

		seq := e.fake.LastSeq()
		if code, raw := e.call("alice"); code != 503 || !strings.Contains(raw, "accounting_unavailable") {
			t.Fatalf("restart %d affected admission: %d %s", restart, code, raw)
		}
		if e.fake.LastSeq() != seq {
			t.Fatalf("restart %d affected admission reached fakellm", restart)
		}

		var bob usage.AggregateResult
		if err := json.Unmarshal(e.aggregate("bob", nil, nil, 200), &bob); err != nil {
			t.Fatal(err)
		}
		wantBob := int64(1 + restart)
		if bob.Totals.Requests != wantBob || bob.Totals.InputTokens != 1000000*wantBob || math.Abs(bob.Totals.EstimatedCost-2*float64(wantBob)) > 1e-9 {
			t.Fatalf("restart %d unrelated aggregate: %+v", restart, bob.Totals)
		}
		status := e.quotaStatus("bob")
		if dig(status, "calibration_error") != nil || dig(status, "groups/models/quotas/daily/used") != float64(2*wantBob) {
			t.Fatalf("restart %d unrelated quota: %+v", restart, status)
		}
		if code, raw := e.call("bob"); code != 200 {
			t.Fatalf("restart %d unrelated admission: %d %s", restart, code, raw)
		}
		if e.fake.LastSeq() == seq {
			t.Fatalf("restart %d unrelated admission did not reach fakellm", restart)
		}
		e.stop()
	}

	// The injected damage must survive restarts: the proxy must not fabricate a
	// replacement raw row, and the owner/quota evidence must remain untouched.
	db, err = bolt.Open(e.dbPath, 0o600, &bolt.Options{ReadOnly: true, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	err = db.View(func(tx *bolt.Tx) error {
		if raw := tx.Bucket([]byte("usage_events")); raw == nil || raw.Get(rawKey) != nil {
			return fmt.Errorf("raw row %q was recreated", rawKey)
		}
		if owners := tx.Bucket([]byte("usage_raw_owners_v1")); owners == nil || owners.Get(rawKey) == nil {
			return fmt.Errorf("raw owner %q was removed", rawKey)
		}
		if quota := tx.Bucket([]byte("quota_usage_agg_hourly")); quota == nil || quota.Get(quotaKey) == nil {
			return fmt.Errorf("quota cell %q was removed", quotaKey)
		}
		return nil
	})
	closeErr = db.Close()
	if err != nil || closeErr != nil {
		t.Fatalf("read back historical evidence: %v / %v", err, closeErr)
	}

}

// TestIssue72RecheckAuditPermission covers the operator path required by the
// issue: an unresolved evidence item can be rechecked idempotently, the audit
// trail records the operation, and a readonly session is rejected and audited.
func TestIssue72RecheckAuditPermission(t *testing.T) {
	e := newIssue72UnknownEnv(t)
	e.start()
	e.configure([]string{"alice"})
	e.seed("alice")
	event := e.waitSingleEvent("alice")
	e.stop()

	event.RecordID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaa0073"
	line := mustJSON(event)
	sidecar := append(mustJSON(map[string]any{
		"rejected_at": time.Now().UTC(), "reason": "identity_unverified",
		"record_id": event.RecordID, "client": "alice", "line": line,
	}), '\n')
	if err := os.WriteFile(e.dbPath+".usage-journal.jsonl.rejected", sidecar, 0o600); err != nil {
		t.Fatal(err)
	}

	e.start()
	var evidence struct {
		Entries []struct {
			ID       string `json:"id"`
			Version  string `json:"version"`
			Resolved bool   `json:"resolved"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(e.admin("GET", "/admin/data/usage/unresolved", nil, 200), &evidence); err != nil {
		t.Fatal(err)
	}
	if len(evidence.Entries) != 1 || evidence.Entries[0].Resolved {
		t.Fatalf("unexpected unresolved evidence: %+v", evidence)
	}
	entry := evidence.Entries[0]
	operationID := "dddddddd-dddd-4ddd-8ddd-dddddddddd72"
	request := map[string]any{"evidence_id": entry.ID, "version": entry.Version, "operation_id": operationID}
	result := decode(t, e.admin("POST", "/admin/data/usage/recheck", request, 200))
	if result["status"] != "insufficient_evidence" {
		t.Fatalf("recheck result: %+v", result)
	}
	if !auditHasEvent(t, e, "usage_recheck", operationID, "") {
		t.Fatal("successful recheck was not audited")
	}

	name := "issue72-readonly"
	const password = "issue72-readonly-password"
	e.admin("POST", "/admin/data/users", map[string]any{"username": name, "password": password, "role": "readonly"}, 201)
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	viewer := &http.Client{Jar: jar, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := viewer.PostForm(e.base+"/login", url.Values{"username": {name}, "password": {password}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		t.Fatalf("readonly login: HTTP %d", resp.StatusCode)
	}
	resp, err = viewer.Post(e.base+"/admin/data/usage/recheck", "application/json", bytes.NewReader(mustJSON(request)))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("readonly recheck: HTTP %d %s", resp.StatusCode, raw)
	}
	if !auditHasEvent(t, e, "usage_recheck", "readonly", "rejected") {
		t.Fatal("readonly recheck rejection was not audited")
	}
}

func auditHasEvent(t *testing.T, e *issue72UnknownEnv, action, detail, result string) bool {
	t.Helper()
	var page struct {
		Events []struct {
			Action string `json:"action"`
			Result string `json:"result"`
			Detail string `json:"detail"`
		} `json:"events"`
	}
	if err := json.Unmarshal(e.admin("GET", "/admin/data/audit?limit=100", nil, 200), &page); err != nil {
		t.Fatal(err)
	}
	for _, event := range page.Events {
		if event.Action == action && event.Detail == detail && (result == "" || event.Result == result) {
			return true
		}
	}
	return false
}
