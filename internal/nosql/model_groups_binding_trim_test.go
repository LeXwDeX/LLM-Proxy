package nosql

import (
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

// Catalog writes normalize binding whitespace so new records never carry a
// target/model with surrounding spaces. Existing records stay readable because
// quota matching trims at compare time.
func TestModelGroupStoreTrimsBindingWhitespaceOnWrite(t *testing.T) {
	db := testDB(t)
	createBindingTarget(t, db) // target "openai-prod", upstream model "gpt-4o"
	groups := NewModelGroupStore(db)

	group := config.ModelGroup{
		Name: "trimmed", CostCoefficient: 1,
		Bindings: []config.QuotaGroupBinding{
			{Target: "  openai-prod  ", Model: " gpt-4o "},
			{Target: "\topenai-prod\n", Model: "   "},
		},
	}
	if err := groups.Create(group); err != nil {
		t.Fatalf("create with whitespace bindings: %v", err)
	}
	stored := storedModelGroup(t, groups, "trimmed")
	if len(stored.Bindings) != 2 {
		t.Fatalf("stored bindings = %+v, want 2", stored.Bindings)
	}
	if stored.Bindings[0].Target != "openai-prod" || stored.Bindings[0].Model != "gpt-4o" {
		t.Fatalf("exact binding not normalized: %+v", stored.Bindings[0])
	}
	if stored.Bindings[1].Target != "openai-prod" || stored.Bindings[1].Model != "" {
		t.Fatalf("whole-target binding not normalized: %+v", stored.Bindings[1])
	}

	updated := config.ModelGroup{
		Name: "trimmed", CostCoefficient: 2,
		Bindings: []config.QuotaGroupBinding{{Target: " openai-prod ", Model: " gpt-4o"}},
	}
	if _, err := groups.Update("trimmed", updated); err != nil {
		t.Fatalf("update with whitespace binding: %v", err)
	}
	stored = storedModelGroup(t, groups, "trimmed")
	if len(stored.Bindings) != 1 || stored.Bindings[0].Target != "openai-prod" || stored.Bindings[0].Model != "gpt-4o" {
		t.Fatalf("updated binding not normalized: %+v", stored.Bindings)
	}
}

func storedModelGroup(t *testing.T, groups *ModelGroupStore, name string) config.ModelGroup {
	t.Helper()
	catalog, err := groups.List()
	if err != nil {
		t.Fatalf("list model groups: %v", err)
	}
	stored := modelGroupByName(catalog, name)
	if stored == nil {
		t.Fatalf("model group %q not stored", name)
	}
	return *stored
}
