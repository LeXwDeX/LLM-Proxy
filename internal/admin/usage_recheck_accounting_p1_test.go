package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/nosql"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	bolt "go.etcd.io/bbolt"
)

func TestIssue72AdminIdentityRecheckReportsAccountingUnresolved(t *testing.T) {
	stores := setupTestStores(t, t.TempDir())
	event := usage.Event{RecordID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaa123", Timestamp: time.Now().UTC(), ClientName: "alice", Model: "model", Target: "target", InputTokens: 100}
	raw, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	if err := stores.usageStore.RegisterUnresolvedUsage(event, raw); err != nil {
		t.Fatal(err)
	}
	if err := stores.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(nosql.BucketUsageEvents)).Put([]byte(event.Timestamp.UTC().Format(time.RFC3339Nano)+"_"+event.RecordID), raw)
	}); err != nil {
		t.Fatal(err)
	}
	entries, _, err := stores.usageStore.ListUsageRechecks(0, 100)
	if err != nil || len(entries) != 1 {
		t.Fatalf("evidence: %v %v", entries, err)
	}
	sessions := NewSessionManager(config.AdminSessionConfig{CookieName: "test", Secret: "test-secret", TTLSeconds: 3600}, nil)
	h := &Handler{usageStore: stores.usageStore, sessionManager: sessions}
	h.router = h.routes()
	handler := sessions.Middleware(h)
	_, cookie, _ := sessions.CreateSession("admin", RoleAdmin)
	body := `{"evidence_id":"` + entries[0].ID + `","version":"` + entries[0].Version + `","operation_id":"dddddddd-dddd-4ddd-8ddd-dddddddddd14"}`
	r := httptest.NewRequest(http.MethodPost, "/data/usage/recheck", strings.NewReader(body))
	r.Header.Set("Accept", "application/json")
	r.AddCookie(cookie)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	var result nosql.UsageRecheckResult
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.Status != "identity_verified_accounting_unresolved" || result.AccountingRestored {
		t.Fatalf("unsafe recheck response: %d %s", w.Code, w.Body.String())
	}
	from, to := event.Timestamp.Truncate(time.Hour), event.Timestamp.Add(time.Hour)
	if _, err := stores.usageStore.ReadCostUnits(context.Background(), usage.Filter{ClientName: event.ClientName, From: &from, To: &to}, usage.CostTable{}, false); !errors.Is(err, nosql.ErrUsageHistoryIncomplete) {
		t.Fatalf("admin recheck cleared accounting block: %v", err)
	}
}
