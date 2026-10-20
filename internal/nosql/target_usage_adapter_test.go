package nosql

import (
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"testing"
)

func TestTargetStoreUsageAdapterValidation(t *testing.T) {
	db := testDB(t)
	store := NewTargetStore(db)
	target := config.Target{Name: "target", Endpoint: "https://example.com", APIKey: "test", UsageAdapter: " QWEN "}
	if err := store.Create(target); err != nil {
		t.Fatal(err)
	}
	targets, err := store.List()
	if err != nil || len(targets) != 1 || targets[0].UsageAdapter != "qwen" {
		t.Fatalf("normalization: %+v %v", targets, err)
	}
	target.UsageAdapter = "invalid"
	if err := store.Update("target", target); err == nil {
		t.Fatal("invalid update accepted")
	}
	target.Name = "bad"
	if err := store.Create(target); err == nil {
		t.Fatal("invalid create accepted")
	}
	targets, err = store.List()
	if err != nil || len(targets) != 1 || targets[0].UsageAdapter != "qwen" {
		t.Fatalf("failed writes changed store: %+v %v", targets, err)
	}
}

func TestTargetMigrationRejectsInvalidUsageAdapter(t *testing.T) {
	db := testDB(t)
	target := config.Target{Name: "target", Endpoint: "https://example.com", APIKey: "test", UsageAdapter: "invalid"}
	if _, err := MigrateTargetsFromConfig(db, []config.Target{target}); err == nil {
		t.Fatal("invalid migration accepted")
	}
	target.UsageAdapter = ""
	if _, err := MigrateTargetsFromConfig(db, []config.Target{target}); err != nil {
		t.Fatal(err)
	}
	targets, err := NewTargetStore(db).List()
	if err != nil || len(targets) != 1 || targets[0].UsageAdapter != "standard" {
		t.Fatalf("legacy auto converted to explicit: %+v %v", targets, err)
	}
}
