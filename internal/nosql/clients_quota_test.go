package nosql

import (
	"sort"
	"testing"

	bolt "go.etcd.io/bbolt"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

func TestClientStoreListWithQuotaEmpty(t *testing.T) {
	db := testDB(t)
	store := NewClientStore(db)

	clients, err := store.ListWithQuota()
	if err != nil {
		t.Fatalf("ListWithQuota: %v", err)
	}
	if len(clients) != 0 {
		t.Errorf("expected empty list, got %d", len(clients))
	}
	if clients == nil {
		t.Error("expected non-nil empty slice")
	}
}

func TestClientStoreListWithQuotaAllZero(t *testing.T) {
	db := testDB(t)
	store := NewClientStore(db)
	if err := store.Create(config.Client{Name: "no-quota", AccessKey: "k1", IPAddress: "192.0.2.1"}); err != nil {
		t.Fatalf("create: %v", err)
	}

	clients, err := store.ListWithQuota()
	if err != nil {
		t.Fatalf("ListWithQuota: %v", err)
	}
	if len(clients) != 0 {
		t.Errorf("all-zero client should not appear, got %d", len(clients))
	}
}

func TestClientStoreListWithQuotaMixed(t *testing.T) {
	db := testDB(t)
	store := NewClientStore(db)

	// 5 clients: 2 with quota, 3 without.
	cases := []config.Client{
		{Name: "a", AccessKey: "k1", IPAddress: "192.0.2.1"},                                            // all zero
		{Name: "b", AccessKey: "k2", IPAddress: "192.0.2.2"},                                            // all zero
		{Name: "c", AccessKey: "k3", IPAddress: "192.0.2.3", QuotaDailyUSD: 10},                         // has quota
		{Name: "d", AccessKey: "k4", IPAddress: "192.0.2.4"},                                            // all zero
		{Name: "e", AccessKey: "k5", IPAddress: "192.0.2.5", QuotaWeeklyUSD: 100, QuotaMonthlyUSD: 500}, // has quota
	}
	for _, c := range cases {
		if err := store.Create(c); err != nil {
			t.Fatalf("create %s: %v", c.Name, err)
		}
	}

	clients, err := store.ListWithQuota()
	if err != nil {
		t.Fatalf("ListWithQuota: %v", err)
	}
	if len(clients) != 2 {
		t.Fatalf("expected 2 clients, got %d: %+v", len(clients), clients)
	}

	names := []string{clients[0].Name, clients[1].Name}
	sort.Strings(names)
	if names[0] != "c" || names[1] != "e" {
		t.Errorf("expected [c, e], got %v", names)
	}
}

func TestClientStoreListWithQuotaAllThree(t *testing.T) {
	db := testDB(t)
	store := NewClientStore(db)

	c := config.Client{
		Name:            "all-three",
		AccessKey:       "k1",
		IPAddress:       "192.0.2.1",
		QuotaDailyUSD:   1,
		QuotaWeeklyUSD:  5,
		QuotaMonthlyUSD: 20,
	}
	if err := store.Create(c); err != nil {
		t.Fatalf("create: %v", err)
	}

	clients, err := store.ListWithQuota()
	if err != nil {
		t.Fatalf("ListWithQuota: %v", err)
	}
	if len(clients) != 1 {
		t.Fatalf("expected 1, got %d", len(clients))
	}
	got := clients[0]
	if got.QuotaDailyUSD != 1 || got.QuotaWeeklyUSD != 5 || got.QuotaMonthlyUSD != 20 {
		t.Errorf("unexpected quota fields: %+v", got)
	}
}

func TestClientStoreListWithQuotaTiny(t *testing.T) {
	db := testDB(t)
	store := NewClientStore(db)

	// 3 clients: zero / tiny-but-positive / normal.
	cases := []config.Client{
		{Name: "zero", AccessKey: "k1", IPAddress: "192.0.2.1"},
		{Name: "tiny", AccessKey: "k2", IPAddress: "192.0.2.2", QuotaDailyUSD: 0.01},
		{Name: "normal", AccessKey: "k3", IPAddress: "192.0.2.3", QuotaWeeklyUSD: 10},
	}
	for _, c := range cases {
		if err := store.Create(c); err != nil {
			t.Fatalf("create %s: %v", c.Name, err)
		}
	}

	clients, err := store.ListWithQuota()
	if err != nil {
		t.Fatalf("ListWithQuota: %v", err)
	}
	if len(clients) != 2 {
		t.Fatalf("expected 2, got %d: %+v", len(clients), clients)
	}

	names := []string{clients[0].Name, clients[1].Name}
	sort.Strings(names)
	if names[0] != "normal" || names[1] != "tiny" {
		t.Errorf("expected [normal, tiny], got %v", names)
	}
}

func TestClientStoreUpdateQuotasPersonalOnly(t *testing.T) {
	db := testDB(t)
	store := NewClientStore(db)
	if err := store.Create(config.Client{Name: "alice", AccessKey: "k1", IPAddress: "192.0.2.1"}); err != nil {
		t.Fatalf("create: %v", err)
	}

	updated, err := store.UpdateQuotas([]string{"alice"}, 1, 2, 3)
	if err != nil {
		t.Fatalf("UpdateQuotas: %v", err)
	}
	if len(updated) != 1 || updated[0] != "alice" {
		t.Fatalf("unexpected updated names: %v", updated)
	}
	clients, err := store.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	got := clients[0]
	if got.QuotaDailyUSD != 1 || got.QuotaWeeklyUSD != 2 || got.QuotaMonthlyUSD != 3 {
		t.Fatalf("quota fields not updated: %+v", got)
	}
	if got.AccessKey != "k1" || got.IPAddress != "192.0.2.1" {
		t.Fatalf("identity fields changed: %+v", got)
	}
}

func createBindingTarget(t *testing.T, db *bolt.DB) {
	t.Helper()
	target := config.Target{
		UsageAdapter: config.UsageAdapterStandard,
		Name:         "openai-prod", EndpointType: "openai", Endpoint: "https://example.com", APIKey: "key",
		ModelMappings: []config.ModelMapping{{Upstream: "gpt-4o", Fallback: "client-alias"}},
	}
	if err := NewTargetStore(db).Create(target); err != nil {
		t.Fatalf("create target: %v", err)
	}
}
