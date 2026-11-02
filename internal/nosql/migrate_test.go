package nosql

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

func TestMigrateFromJSON(t *testing.T) {
	dir := t.TempDir()

	// Prepare old JSON files.
	clients := []config.Client{
		{Name: "team-a", AccessKey: "k1"},
		{Name: "team-b", AccessKey: "k2"},
	}
	writeJSONFile(t, filepath.Join(dir, "clients.json"), clients)

	costs := []ModelCost{
		{EndpointType: "azure_openai", Model: "gpt-4o", InputPer1MTokens: 0.1, OutputPer1MTokens: 0.2},
	}
	writeJSONFile(t, filepath.Join(dir, "model_costs.json"), costs)

	users := []config.AdminUser{
		{Username: "admin", PasswordHash: "sha256$salt$hash", Role: "admin"},
	}
	writeJSONFile(t, filepath.Join(dir, "admin_users.json"), users)

	// JSONL files.
	writeJSONLFile(t, filepath.Join(dir, "usage_events.jsonl"), []usage.Event{
		{Timestamp: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), ClientName: "team-a", Model: "gpt-4o", InputTokens: 100},
	})
	writeJSONLFile(t, filepath.Join(dir, "admin_audit.jsonl"), []AuditEvent{
		{ID: "1", Timestamp: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Actor: "admin", Action: "test", Result: "ok"},
	})

	// Open DB and migrate.
	db := testDB(t)
	dataFiles := config.DataFiles{
		ClientsFile:     filepath.Join(dir, "clients.json"),
		ModelCostsFile:  filepath.Join(dir, "model_costs.json"),
		UsageEventsFile: filepath.Join(dir, "usage_events.jsonl"),
		AdminUsersFile:  filepath.Join(dir, "admin_users.json"),
		AdminAuditFile:  filepath.Join(dir, "admin_audit.jsonl"),
	}

	if err := MigrateFromJSON(db, dataFiles); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// Verify clients.
	clientStore := NewClientStore(db)
	cl, err := clientStore.List()
	if err != nil {
		t.Fatalf("list clients: %v", err)
	}
	if len(cl) != 2 {
		t.Fatalf("expected 2 clients, got %d", len(cl))
	}

	// Verify model costs.
	costStore := NewModelCostStore(db)
	mc, err := costStore.List()
	if err != nil {
		t.Fatalf("list model costs: %v", err)
	}
	if len(mc) != 1 {
		t.Fatalf("expected 1 model cost, got %d", len(mc))
	}

	// Verify usage events.
	usageStore := NewUsageStore(db)
	ue, err := usageStore.List(usage.Filter{})
	if err != nil {
		t.Fatalf("list usage events: %v", err)
	}
	if len(ue) != 1 {
		t.Fatalf("expected 1 usage event, got %d", len(ue))
	}

	// Verify admin users.
	userStore := NewUserStore(db)
	au, err := userStore.List()
	if err != nil {
		t.Fatalf("list admin users: %v", err)
	}
	if len(au) != 1 {
		t.Fatalf("expected 1 admin user, got %d", len(au))
	}

	// Verify audit events.
	auditStore := NewAuditStore(db)
	ae, err := auditStore.List(10)
	if err != nil {
		t.Fatalf("list audit events: %v", err)
	}
	if len(ae) != 1 {
		t.Fatalf("expected 1 audit event, got %d", len(ae))
	}

	// Verify migration marker.
	var migrated bool
	_ = db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketMeta))
		if b.Get([]byte("migrated")) != nil {
			migrated = true
		}
		return nil
	})
	if !migrated {
		t.Fatalf("expected migration marker")
	}
}

func TestMigrateIdempotent(t *testing.T) {
	dir := t.TempDir()
	clients := []config.Client{{Name: "team-a", AccessKey: "k1"}}
	writeJSONFile(t, filepath.Join(dir, "clients.json"), clients)

	db := testDB(t)
	dataFiles := config.DataFiles{
		ClientsFile: filepath.Join(dir, "clients.json"),
	}

	// First migration.
	if err := MigrateFromJSON(db, dataFiles); err != nil {
		t.Fatalf("first migrate: %v", err)
	}

	// Second migration should be a no-op.
	if err := MigrateFromJSON(db, dataFiles); err != nil {
		t.Fatalf("second migrate: %v", err)
	}

	// Verify only 1 client exists (not duplicated).
	clientStore := NewClientStore(db)
	cl, err := clientStore.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(cl) != 1 {
		t.Fatalf("expected 1 client after idempotent migrate, got %d", len(cl))
	}
}

func TestMigrateSkipsExisting(t *testing.T) {
	dir := t.TempDir()
	clients := []config.Client{{Name: "from-json", AccessKey: "k1"}}
	writeJSONFile(t, filepath.Join(dir, "clients.json"), clients)

	db := testDB(t)

	// Pre-populate the bucket.
	clientStore := NewClientStore(db)
	if err := clientStore.Create(config.Client{Name: "existing", AccessKey: "k0", IPAddress: "192.0.2.1"}); err != nil {
		t.Fatalf("pre-create: %v", err)
	}

	dataFiles := config.DataFiles{
		ClientsFile: filepath.Join(dir, "clients.json"),
	}

	if err := MigrateFromJSON(db, dataFiles); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// The JSON client should NOT have been imported (bucket already had data).
	cl, err := clientStore.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(cl) != 1 || cl[0].Name != "existing" {
		t.Fatalf("expected only pre-existing client, got %+v", cl)
	}
}

// Issue #142: legacy clients.json may contain the reserved "unknown" name.
// Import must skip just those rows, keep the remaining clients, and stay
// idempotent.
func TestMigrateClientsSkipsReservedUnknownNameIssue142(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "clients.json")
	writeJSONFile(t, path, []config.Client{
		{Name: "unknown", AccessKey: "k0"},
		{Name: "Unknown", AccessKey: "k1"},
		{Name: "team-a", AccessKey: "k2"},
		{Name: "unknown-2", AccessKey: "k3"},
	})

	db := testDB(t)
	dataFiles := config.DataFiles{ClientsFile: path}
	if err := MigrateFromJSON(db, dataFiles); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	store := NewClientStore(db)
	clients, err := store.List()
	if err != nil {
		t.Fatalf("list clients: %v", err)
	}
	if len(clients) != 2 {
		t.Fatalf("expected 2 imported clients, got %+v", clients)
	}
	names := make(map[string]bool, len(clients))
	for _, c := range clients {
		names[strings.ToLower(c.Name)] = true
	}
	if !names["team-a"] || !names["unknown-2"] {
		t.Fatalf("legal clients missing after migration: %+v", clients)
	}
	for _, reserved := range []string{"unknown", "UNKNOWN"} {
		got, err := store.Get(reserved)
		if err != nil {
			t.Fatalf("get reserved name %q: %v", reserved, err)
		}
		if got != nil {
			t.Fatalf("reserved client %q was imported: %+v", reserved, got)
		}
	}

	// Re-running the migration is a no-op: no error, no duplicates.
	if err := MigrateFromJSON(db, dataFiles); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	clients, err = store.List()
	if err != nil {
		t.Fatalf("list clients after second migrate: %v", err)
	}
	if len(clients) != 2 {
		t.Fatalf("expected 2 clients after second migrate, got %+v", clients)
	}
}

// --- Test helpers ---

func writeJSONFile(t *testing.T, path string, v interface{}) {
	t.Helper()
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func writeJSONLFile(t *testing.T, path string, items interface{}) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	defer f.Close()

	// Items is expected to be a slice; iterate via reflection or type switch.
	switch v := items.(type) {
	case []usage.Event:
		for _, item := range v {
			data, err := json.Marshal(item)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			f.Write(append(data, '\n'))
		}
	case []AuditEvent:
		for _, item := range v {
			data, err := json.Marshal(item)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			f.Write(append(data, '\n'))
		}
	default:
		t.Fatalf("unsupported type for writeJSONLFile")
	}
}
