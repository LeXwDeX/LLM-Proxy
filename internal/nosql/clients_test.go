package nosql

import (
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

func TestClientStoreCRUD(t *testing.T) {
	db := testDB(t)
	store := NewClientStore(db)

	if err := store.Create(config.Client{Name: "team-a", AccessKey: "k1", IPAddress: "192.0.2.1"}); err != nil {
		t.Fatalf("create client: %v", err)
	}
	if err := store.Create(config.Client{Name: "team-a", AccessKey: "k2", IPAddress: "192.0.2.2"}); err == nil {
		t.Fatalf("expected duplicate name error")
	}

	clients, err := store.List()
	if err != nil {
		t.Fatalf("list clients: %v", err)
	}
	if len(clients) != 1 || clients[0].Name != "team-a" || clients[0].IPAddress != "192.0.2.1" {
		t.Fatalf("unexpected clients: %+v", clients)
	}

	if err := store.Update("team-a", config.Client{Name: "team-b", AccessKey: "k2", IPAddress: "198.51.100.2", AllowedTargets: []string{"Primary"}}); err != nil {
		t.Fatalf("update client: %v", err)
	}

	clients, err = store.List()
	if err != nil {
		t.Fatalf("list after update: %v", err)
	}
	if len(clients) != 1 || clients[0].Name != "team-b" || clients[0].IPAddress != "198.51.100.2" || clients[0].AllowedTargets[0] != "primary" {
		t.Fatalf("unexpected updated clients: %+v", clients)
	}

	if err := store.Delete("team-b"); err != nil {
		t.Fatalf("delete client: %v", err)
	}

	clients, err = store.List()
	if err != nil {
		t.Fatalf("list after delete: %v", err)
	}
	if len(clients) != 0 {
		t.Fatalf("expected no clients, got %+v", clients)
	}
}

func TestClientStoreAccessKeyUniqueness(t *testing.T) {
	db := testDB(t)
	store := NewClientStore(db)

	if err := store.Create(config.Client{Name: "a", AccessKey: "k1", IPAddress: "192.0.2.1"}); err != nil {
		t.Fatalf("create a: %v", err)
	}
	if err := store.Create(config.Client{Name: "b", AccessKey: "k1", IPAddress: "192.0.2.2"}); err == nil {
		t.Fatalf("expected duplicate access_key error")
	}
}

func TestClientStoreCaseInsensitive(t *testing.T) {
	db := testDB(t)
	store := NewClientStore(db)

	if err := store.Create(config.Client{Name: "Team-A", AccessKey: "k1", IPAddress: "192.0.2.1"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	// Creating with different case should fail.
	if err := store.Create(config.Client{Name: "team-a", AccessKey: "k2", IPAddress: "192.0.2.2"}); err == nil {
		t.Fatalf("expected case-insensitive duplicate name error")
	}

	// Update with different case should work.
	if err := store.Update("TEAM-A", config.Client{Name: "Team-B", AccessKey: "k2", IPAddress: "198.51.100.2"}); err != nil {
		t.Fatalf("update with different case: %v", err)
	}

	// Delete with different case should work.
	if err := store.Delete("TEAM-B"); err != nil {
		t.Fatalf("delete with different case: %v", err)
	}
}

func TestClientStoreGet(t *testing.T) {
	store := NewClientStore(testDB(t))
	if got, err := store.Get("nobody"); err != nil || got != nil {
		t.Fatalf("missing client: got %+v, err %v", got, err)
	}
	if err := store.Create(config.Client{Name: "Team-A", AccessKey: "k1", IPAddress: "192.0.2.1", JobLevel: "L5", ProjectGroup: "AI"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := store.Get("TEAM-a")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got == nil || got.Name != "Team-A" || got.JobLevel != "L5" || got.ProjectGroup != "AI" {
		t.Fatalf("unexpected client: %+v", got)
	}
}

func TestClientStoreAcceptsIPv4AddressAndCIDRList(t *testing.T) {
	store := NewClientStore(testDB(t))
	if err := store.Create(config.Client{Name: "empty", AccessKey: "key-empty"}); err != nil {
		t.Fatalf("expected empty ip_address to be accepted: %v", err)
	}
	if err := store.Create(config.Client{Name: "list", AccessKey: "key-list", IPAddress: "192.168.0.0/16, 192.168.50.17/24\n10.0.1.1"}); err != nil {
		t.Fatalf("expected IPv4/CIDR list to be accepted: %v", err)
	}
	client, err := store.Get("list")
	if err != nil || client.IPAddress != "192.168.0.0/16,192.168.50.0/24,10.0.1.1" {
		t.Fatalf("stored list = %+v, %v", client, err)
	}
	if err := store.Update("list", config.Client{Name: "list", AccessKey: "key-list", IPAddress: "192.168.2.150,192.168.50.83"}); err != nil {
		t.Fatalf("update list: %v", err)
	}
	client, err = store.Get("list")
	if err != nil || client.IPAddress != "192.168.2.150,192.168.50.83" {
		t.Fatalf("updated list = %+v, %v", client, err)
	}
	for _, tc := range []struct {
		name      string
		ipAddress string
	}{
		{name: "invalid", ipAddress: "192.168.1.999"},
		{name: "ipv6", ipAddress: "2001:db8::1"},
		{name: "ipv4-mapped-ipv6", ipAddress: "::ffff:192.0.2.1"},
		{name: "invalid-cidr", ipAddress: "192.168.0.0/33"},
		{name: "empty-entry", ipAddress: "192.168.0.0/16,,10.0.1.1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := store.Create(config.Client{Name: tc.name, AccessKey: "key-" + tc.name, IPAddress: tc.ipAddress})
			if err == nil {
				t.Fatal("expected invalid IPv4 address to be rejected")
			}
		})
	}
}

func TestClientStoreRenameMigratesQuotaEntry(t *testing.T) {
	db := testDB(t)
	clientStore := NewClientStore(db)
	quotaStore := NewClientQuotaStore(db)
	seedModelGroups(t, db, "premium")

	if err := clientStore.Create(config.Client{Name: "alice", AccessKey: "k1", IPAddress: "192.0.2.1"}); err != nil {
		t.Fatalf("create client: %v", err)
	}
	if err := quotaStore.Replace("alice", []config.QuotaGroup{{Name: "premium", Coefficient: coefPtr(0.8)}}); err != nil {
		t.Fatalf("replace quota groups: %v", err)
	}
	if err := clientStore.Update("alice", config.Client{Name: "alice-renamed", AccessKey: "k2", IPAddress: "192.0.2.2"}); err != nil {
		t.Fatalf("rename client: %v", err)
	}

	got, err := quotaStore.Get("alice-renamed")
	if err != nil {
		t.Fatalf("get renamed quota groups: %v", err)
	}
	if len(got) != 1 || got[0].Name != "premium" || got[0].PlanCoefficient() != 0.8 {
		t.Fatalf("quota entry not migrated: %+v", got)
	}
	old, err := quotaStore.Get("alice")
	if err != nil {
		t.Fatalf("get old quota groups: %v", err)
	}
	if len(old) != 0 {
		t.Fatalf("old quota entry should be gone: %+v", old)
	}
}
