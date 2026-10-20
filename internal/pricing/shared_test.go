package pricing

import (
	"encoding/json"
	"testing"
	"time"
)

func sharedRule(t *testing.T, body string) PricingTemplate {
	t.Helper()
	var p PricingTemplate
	if err := json.Unmarshal([]byte(body), &p); err != nil {
		t.Fatal(err)
	}
	NormalizeTemplate(&p)
	return p
}

func TestSharedRuleResolvesEveryBindingAndHistoricalWindow(t *testing.T) {
	base := sharedRule(t, `{"targets":["A","B"],"models":["fable5","fable5-1"],"base_rates":{"input_per_1m_tokens":2,"image_input_per_1m_tokens":0}}`)
	window := base
	window.Start = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := window.Start.Add(time.Hour)
	window.End = &end
	window.BaseRates.InputPer1MTokens = 7
	r := NewResolver([]PricingTemplate{base, window}, nil)
	for _, target := range []string{"a", "B"} {
		for _, model := range []string{"fable5", "FABLE5-1"} {
			for _, tc := range []struct {
				at   time.Time
				want float64
			}{{window.Start.Add(-time.Second), 2}, {window.Start, 7}, {end, 2}} {
				rates, ok := r.RateFor(target, model, tc.at)
				if !ok || rates.InputPer1MTokens != tc.want {
					t.Fatalf("%s/%s: got %+v, %v", target, model, rates, ok)
				}
				if rates.ImageInputPer1MTokens == nil || *rates.ImageInputPer1MTokens != 0 || rates.CacheWrite1hPer1MToken != nil {
					t.Fatal("optional zero/nil lost")
				}
			}
		}
	}
	if _, ok := r.RateFor("C", "fable5", end); ok {
		t.Fatal("unbound target priced")
	}
}

func TestSharedCoverageIntersections(t *testing.T) {
	a := sharedRule(t, `{"targets":["A","B"],"models":["fable5","fable5-1"],"start":"2026-01-01T00:00:00Z"}`)
	for _, tc := range []struct {
		body     string
		conflict bool
	}{
		{`{"targets":["B","C"],"models":["fable5-1","x"],"start":"2026-01-02T00:00:00Z"}`, true},
		{`{"target":"B","model":"different","start":"2026-01-02T00:00:00Z"}`, false},
		{`{"target":"C","model":"fable5","start":"2026-01-02T00:00:00Z"}`, false},
	} {
		err := ValidateCoverageSet([]PricingTemplate{a, sharedRule(t, tc.body)})
		if (err != nil) != tc.conflict {
			t.Fatalf("conflict %v got %v", tc.conflict, err)
		}
	}
}

func TestSharedBindingsValidationAndSetIdentity(t *testing.T) {
	for _, body := range []string{
		`{"targets":[],"models":["m"]}`,
		`{"targets":["a"],"models":[]}`,
		`{"targets":["","a"],"models":["m"]}`,
		`{"targets":["a"],"models":[""]}`,
		`{"target":"b","targets":["a"],"models":["m"]}`,
		`{"targets":["a"],"model":"other","models":["m"]}`,
	} {
		var p PricingTemplate
		if err := json.Unmarshal([]byte(body), &p); err != nil {
			t.Fatal(err)
		}
		if err := ValidateBindings(&p); err == nil {
			t.Fatalf("accepted %s", body)
		}
	}
	a := sharedRule(t, `{"targets":[" A ","B","a"],"models":["Fable5","fable5-1"]}`)
	b := sharedRule(t, `{"targets":["b","a"],"models":["FABLE5-1","fable5"]}`)
	if !a.SameBindings(b) || len(a.EffectiveTargets()) != 2 {
		t.Fatal("case/order/dedup binding semantics changed")
	}
	r := NewResolver([]PricingTemplate{a}, nil)
	a.Targets[0] = "changed"
	a.Models[0] = "changed"
	if _, ok := r.RateFor("A", "Fable5", time.Now()); !ok {
		t.Fatal("resolver retained caller-owned bindings")
	}
}

func TestSharedGlobalFallbackAndExactEventTime(t *testing.T) {
	global := sharedRule(t, `{"targets":[""],"models":["m","n"],"base_rates":{"input_per_1m_tokens":3}}`)
	scoped := sharedRule(t, `{"targets":["a","b"],"models":["m","n"],"start":"2026-01-01T00:15:00Z","end":"2026-01-01T00:45:00Z","base_rates":{"input_per_1m_tokens":0}}`)
	r := NewResolver([]PricingTemplate{global, scoped}, nil)
	for _, target := range []string{"a", "b"} {
		for _, model := range []string{"m", "n"} {
			if !r.RequiresEventTimestampsFor(target, model) {
				t.Fatal("shared window lost exact event timestamp requirement")
			}
			zero, ok := r.RateFor(target, model, scoped.Start)
			if !ok || zero.InputPer1MTokens != 0 {
				t.Fatal("explicit zero did not override global fallback")
			}
			after, ok := r.RateFor(target, model, *scoped.End)
			if !ok || after.InputPer1MTokens != 3 {
				t.Fatal("global fallback missing after window")
			}
		}
	}
}
