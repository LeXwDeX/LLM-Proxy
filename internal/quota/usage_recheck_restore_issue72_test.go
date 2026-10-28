package quota

import (
	"encoding/json"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/nosql"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	bolt "go.etcd.io/bbolt"
)

func TestIssue72RecheckRestoredBundleAllowsQuotaCalibration(t *testing.T) {
	m := bundleManager(t, 1)
	event := quotaBundle()
	if err := m.RecordUsage(m.usageStore, event); err != nil {
		t.Fatal(err)
	}
	if got := bundleUsed(m); got != 12 {
		t.Fatalf("initial spend %v", got)
	}
	rows, err := m.usageStore.List(usage.Filter{ClientName: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	var root usage.Event
	for _, row := range rows {
		if row.RecordID == event.RecordID {
			root = row
		}
	}
	if root.RecordID == "" {
		t.Fatal("committed root missing")
	}
	original, _ := json.Marshal(root)
	entry, _ := json.Marshal(struct {
		Event    usage.Event `json:"event"`
		Original []byte      `json:"original"`
	}{root, original})
	db := m.clientStore.DB()
	if err := db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte("usage_unresolved_events_v1"))
		if err != nil {
			return err
		}
		if err := tx.Bucket([]byte(nosql.BucketMeta)).Put([]byte("usage_unresolved_events_initialized_v1"), []byte("1")); err != nil {
			return err
		}
		return b.Put([]byte("fixture"), entry)
	}); err != nil {
		t.Fatal(err)
	}
	m.Evaluate("alice")
	if status := m.Status("alice"); status.CalibrationError == "" || bundleUsed(m) != 12 {
		t.Fatalf("expected blocked calibration: %+v", status)
	}
	if _, blocked := m.CheckTarget("alice", "anthropic", "executor"); !blocked {
		t.Fatal("unresolved evidence allowed admission")
	}
	evidence, _, err := m.usageStore.ListUsageRechecks(0, 100)
	if err != nil || len(evidence) != 1 {
		t.Fatalf("evidence: %+v %v", evidence, err)
	}
	result, err := m.usageStore.RecheckUsage(nosql.UsageRecheckRequest{EvidenceID: evidence[0].ID, Version: evidence[0].Version, OperationID: "42345678-1234-4234-8234-123456789abc", Actor: "admin"})
	if err != nil || !result.AccountingVerified || !result.AccountingRestored {
		t.Fatalf("recheck: %+v %v", result, err)
	}
	m.Evaluate("alice")
	if status := m.Status("alice"); status.CalibrationError != "" || bundleUsed(m) != 12 {
		t.Fatalf("calibration changed charge or stayed blocked: %+v", status)
	}
	if info, blocked := m.CheckTarget("alice", "anthropic", "executor"); blocked {
		t.Fatalf("restored accounting still blocked: %+v", info)
	}
}
