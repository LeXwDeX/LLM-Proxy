package nosql

import (
	"encoding/json"
	"github.com/LeXwDeX/LLM-Proxy/internal/pricing"
	"testing"
)

func TestSharedPricingSingleRecordAtomicConflict(t *testing.T) {
	store := NewPricingTemplateStore(testDB(t))
	var rule pricing.PricingTemplate
	if err := json.Unmarshal([]byte(`{"targets":["A","B"],"models":["fable5","fable5-1"],"base_rates":{"input_per_1m_tokens":3}}`), &rule); err != nil {
		t.Fatal(err)
	}
	stored, err := store.Upsert(rule)
	if err != nil {
		t.Fatal(err)
	}
	list, err := store.ListByTargetModel("b", "FABLE5-1")
	if err != nil || len(list) != 1 || list[0].ID != stored.ID {
		t.Fatalf("lookup %+v %v", list, err)
	}
	list, err = store.List()
	if err != nil || len(list) != 1 {
		t.Fatalf("must store one shared record %+v %v", list, err)
	}
	if _, err = store.Upsert(pricing.PricingTemplate{Target: "B", Model: "fable5-1"}); err == nil {
		t.Fatal("intersecting template zero accepted")
	}
	list, _ = store.List()
	if len(list) != 1 {
		t.Fatal("failed write left partial rows")
	}
	stored.BaseRates.InputPer1MTokens = 9
	mustUpsert(t, store, stored)
	list, _ = store.ListByTargetModel("A", "fable5")
	if list[0].BaseRates.InputPer1MTokens != 9 {
		t.Fatal("shared update missing")
	}
	if err = store.Delete(stored.ID); err != nil {
		t.Fatal(err)
	}
	list, _ = store.ListByTargetModel("B", "fable5-1")
	if len(list) != 0 {
		t.Fatal("delete left binding")
	}
}

func TestSharedPricingCoverageWriteIsAtomic(t *testing.T) {
	store := NewPricingTemplateStore(testDB(t))
	parse := func(body string) pricing.PricingTemplate {
		var rule pricing.PricingTemplate
		if err := json.Unmarshal([]byte(body), &rule); err != nil {
			t.Fatal(err)
		}
		return rule
	}
	first := mustUpsert(t, store, parse(`{"targets":["A","B"],"models":["m","n"],"start":"2026-01-01T00:00:00Z","end":"2026-02-01T00:00:00Z"}`))
	if _, err := store.Upsert(parse(`{"targets":["C","B"],"models":["other","n"],"start":"2026-01-02T00:00:00Z"}`)); err == nil {
		t.Fatal("cross-binding coverage overlap accepted")
	}
	list, _ := store.List()
	if len(list) != 1 || list[0].ID != first.ID {
		t.Fatalf("conflict changed records: %+v", list)
	}
	mustUpsert(t, store, parse(`{"targets":["A","B"],"models":["m","n"],"start":"2026-02-01T00:00:00Z"}`))
	list, _ = store.ListByTargetModel("B", "n")
	if len(list) != 2 {
		t.Fatal("adjacent shared window missing")
	}
}
