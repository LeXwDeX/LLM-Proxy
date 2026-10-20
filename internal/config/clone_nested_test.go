package config

import "testing"

func TestManagerSnapshotsIsolateNestedCustomBody(t *testing.T) {
	cfg := &Config{Targets: []Target{{CustomBody: map[string]any{
		"options": map[string]any{"values": []any{map[string]any{"flag": true}}},
		"empty":   map[string]any{},
	}}}}
	manager := NewManager("")
	manager.Replace(cfg)
	original := cfg.Targets[0].CustomBody["options"].(map[string]any)["values"].([]any)[0].(map[string]any)
	original["flag"] = false
	first, err := manager.Current()
	if err != nil {
		t.Fatal(err)
	}
	flag := first.Targets[0].CustomBody["options"].(map[string]any)["values"].([]any)[0].(map[string]any)
	if flag["flag"] != true {
		t.Fatal("Replace retained caller's nested JSON map")
	}
	flag["flag"] = false
	first.Targets[0].CustomBody["empty"].(map[string]any)["new"] = "changed"
	second, err := manager.Current()
	if err != nil {
		t.Fatal(err)
	}
	if second.Targets[0].CustomBody["options"].(map[string]any)["values"].([]any)[0].(map[string]any)["flag"] != true {
		t.Fatal("Current leaked nested JSON mutations into manager cache")
	}
	if len(second.Targets[0].CustomBody["empty"].(map[string]any)) != 0 {
		t.Fatal("empty nested map was shared")
	}
}
