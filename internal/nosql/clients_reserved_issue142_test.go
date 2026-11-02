package nosql

import (
	"fmt"
	"strings"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

// Issue #142: "unknown" is the usage ledger's sentinel for unattributed rows.
// A real client with that name can never be covered by the freshness proof, so
// new writes must reject it (existing rows are left untouched).
func TestClientStoreRejectsReservedUnknownNameIssue142(t *testing.T) {
	store := NewClientStore(testDB(t))

	for i, name := range []string{"unknown", "Unknown", "UNKNOWN", "  UnKnOwN  "} {
		err := store.Create(config.Client{Name: name, AccessKey: fmt.Sprintf("key-%d", i)})
		if err == nil {
			t.Fatalf("Create accepted reserved client name %q", name)
		}
		if !strings.Contains(err.Error(), "reserved") {
			t.Fatalf("Create(%q) error = %q, want reserved-name error", name, err)
		}
	}

	clients, err := store.List()
	if err != nil {
		t.Fatalf("list clients: %v", err)
	}
	if len(clients) != 0 {
		t.Fatalf("reserved names must not be persisted, got %+v", clients)
	}

	if err := store.Create(config.Client{Name: "team-a", AccessKey: "key-a", IPAddress: "192.0.2.1"}); err != nil {
		t.Fatalf("create legal client: %v", err)
	}
	for _, name := range []string{"unknown", "Unknown", "UNKNOWN"} {
		err := store.Update("team-a", config.Client{Name: name, AccessKey: "key-b", IPAddress: "192.0.2.1"})
		if err == nil {
			t.Fatalf("Update accepted reserved client name %q", name)
		}
		if !strings.Contains(err.Error(), "reserved") {
			t.Fatalf("Update to %q error = %q, want reserved-name error", name, err)
		}
	}

	// The rejected updates must not have changed the existing client.
	got, err := store.Get("team-a")
	if err != nil {
		t.Fatalf("get legal client: %v", err)
	}
	if got == nil || got.Name != "team-a" || got.AccessKey != "key-a" {
		t.Fatalf("legal client changed after rejected updates: %+v", got)
	}
}

func TestClientStoreAllowsNamesContainingUnknownIssue142(t *testing.T) {
	store := NewClientStore(testDB(t))

	for i, name := range []string{"unknown-2", "my-unknown", "unknown_client", "unknowns"} {
		if err := store.Create(config.Client{Name: name, AccessKey: fmt.Sprintf("key-%d", i)}); err != nil {
			t.Fatalf("create client %q: %v", name, err)
		}
		got, err := store.Get(name)
		if err != nil {
			t.Fatalf("get client %q: %v", name, err)
		}
		if got == nil || got.Name != name {
			t.Fatalf("client %q not stored correctly: %+v", name, got)
		}
	}
}
