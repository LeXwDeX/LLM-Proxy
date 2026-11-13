package nosql

import (
	"reflect"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

// 删除目标时，同一事务移除所有模型组中引用它的绑定（整目标与精确模型），
// 其他目标的绑定与组的其余字段保持不变。
func TestTargetDeleteRemovesModelGroupBindings(t *testing.T) {
	for _, conditional := range []bool{false, true} {
		env := newRenameEnv(t, "Gone", "kept")
		if err := env.groups.Create(config.ModelGroup{Name: "g1", CostCoefficient: 0.5, DailyUSD: 7, Bindings: []config.QuotaGroupBinding{
			{Target: "Gone"}, {Target: "kept", Model: "m1"}, {Target: "gone", Model: "m1"},
		}}); err != nil {
			t.Fatal(err)
		}
		if err := env.groups.Create(config.ModelGroup{Name: "g2", CostCoefficient: 1, Bindings: []config.QuotaGroupBinding{{Target: "kept"}}}); err != nil {
			t.Fatal(err)
		}
		var err error
		if conditional {
			etag, etagErr := env.targets.ETag("Gone")
			if etagErr != nil {
				t.Fatal(etagErr)
			}
			err = env.targets.DeleteIfMatch("Gone", etag)
		} else {
			err = env.targets.Delete("Gone")
		}
		if err != nil {
			t.Fatal(err)
		}
		groups, err := env.groups.List()
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]config.ModelGroup{}
		for _, g := range groups {
			got[g.Name] = g
		}
		if want := []config.QuotaGroupBinding{{Target: "kept", Model: "m1"}}; !reflect.DeepEqual(got["g1"].Bindings, want) {
			t.Fatalf("conditional=%v g1 bindings = %+v, want %+v", conditional, got["g1"].Bindings, want)
		}
		if got["g1"].CostCoefficient != 0.5 || got["g1"].DailyUSD != 7 {
			t.Fatalf("g1 other fields changed: %+v", got["g1"])
		}
		if want := []config.QuotaGroupBinding{{Target: "kept"}}; !reflect.DeepEqual(got["g2"].Bindings, want) {
			t.Fatalf("g2 bindings = %+v, want untouched", got["g2"].Bindings)
		}
	}
}

// 删除失败（ETag 冲突）时绑定不动。
func TestTargetDeleteConflictKeepsModelGroupBindings(t *testing.T) {
	env := newRenameEnv(t, "gone")
	if err := env.groups.Create(config.ModelGroup{Name: "g1", CostCoefficient: 1, Bindings: []config.QuotaGroupBinding{{Target: "gone"}}}); err != nil {
		t.Fatal(err)
	}
	if err := env.targets.DeleteIfMatch("gone", "t1-stale"); err == nil {
		t.Fatal("stale etag delete succeeded")
	}
	groups, err := env.groups.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || len(groups[0].Bindings) != 1 {
		t.Fatalf("bindings changed after failed delete: %+v", groups)
	}
}
