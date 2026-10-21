package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/admission"
	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/nosql"
	"github.com/LeXwDeX/LLM-Proxy/internal/quota"
	bolt "go.etcd.io/bbolt"
)

// Each test owns its database; group A is billed independently of executor B.
func supplementaryQuota(t *testing.T, targets []string, groupFactor, rankFactor, personalFactor float64, spent bool) *quota.Manager {
	t.Helper()
	db, err := nosql.OpenDB(filepath.Join(t.TempDir(), "quota.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	put := func(bucket, key string, value any) {
		t.Helper()
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.Update(func(tx *bolt.Tx) error { return tx.Bucket([]byte(bucket)).Put([]byte(key), raw) }); err != nil {
			t.Fatal(err)
		}
	}
	put(nosql.BucketClients, "tester", config.Client{Name: "tester", AccessKey: "test-key", JobLevel: "test-level"})
	put(nosql.BucketJobLevels, "test-level", config.JobLevel{Name: "test-level", CoefficientDaily: &rankFactor})
	var bindings []config.QuotaGroupBinding
	for _, target := range targets {
		for _, model := range []string{"Advisor-Model", "Fallback-Model"} {
			bindings = append(bindings, config.QuotaGroupBinding{Target: target, Model: model})
		}
	}
	put(nosql.BucketModelGroups, "a", config.ModelGroup{Name: "a", DailyUSD: 1, CostCoefficient: groupFactor, Bindings: bindings})
	put(nosql.BucketModelGroups, "b", config.ModelGroup{Name: "b", DailyUSD: 1, CostCoefficient: 1,
		Bindings: []config.QuotaGroupBinding{{Target: targets[0], Model: "Executor-Model"}}})
	put(nosql.BucketClientQuotaGroups, "tester", struct {
		Name   string              `json:"name"`
		Groups []config.QuotaGroup `json:"groups"`
	}{"tester", []config.QuotaGroup{{Name: "a", Coefficient: &personalFactor}, {Name: "b"}}})
	costs := nosql.NewModelCostStore(db)
	if err := costs.Upsert(nosql.ModelCost{Model: "Advisor-Model", InputPer1MTokens: 10}); err != nil {
		t.Fatal(err)
	}
	m, err := quota.New(quota.Options{CostStore: costs, UsageStore: nosql.NewUsageStore(db), ClientStore: nosql.NewClientStore(db),
		ModelGroupStore: nosql.NewModelGroupStore(db), ClientQuotaStore: nosql.NewClientQuotaStore(db), JobLevelStore: nosql.NewJobLevelStore(db), Logger: newTestLogger()})
	if err != nil {
		t.Fatal(err)
	}
	m.Evaluate("tester")
	if spent {
		m.IncrementForTarget("tester", targets[0], "claude", "Advisor-Model", time.Now().UTC(), 1_000_000, 0, 0, 0)
	}
	return m
}

func supplementaryRequest(t *testing.T, svc *Service, path, fields string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"model":"executor","max_tokens":32,"messages":[]`+fields+`}`))
	r.Header.Set("Content-Type", "application/json")
	r = r.WithContext(auth.WithPrincipal(r.Context(), newAllowAllPrincipal(t, "test-key")))
	w := httptest.NewRecorder()
	svc.ServeHTTP(w, r)
	return w
}

func TestSupplementaryAdmissionExhaustedGroup(t *testing.T) {
	for _, tc := range []struct{ name, fields string }{
		{"advisor", `,"tools":[{"type":"advisor_20260301","name":"advisor","model":"Advisor-Model"}]`},
		{"fallback", `,"fallback_model":"Fallback-Model"`},
		{"official first", `,"fallbacks":[{"model":"Fallback-Model"},{"model":"Unbound-Model"}]`},
		{"official later", `,"fallbacks":[{"model":"Unbound-Model"},{"model":"Fallback-Model"}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var hits atomic.Int64
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"type":"message","model":"Executor-Model","content":[],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
			}))
			defer upstream.Close()
			svc, err := NewService(&config.Config{Targets: []config.Target{{Name: "upstream", EndpointType: "claude", Endpoint: upstream.URL,
				APIKeys: []string{"test-upstream-key", "test-upstream-key-2"}, ModelMappings: []config.ModelMapping{{Upstream: "Executor-Model", Fallback: "executor"}}}}}, newTestLogger())
			if err != nil {
				t.Fatal(err)
			}
			defer svc.Close()
			qm := supplementaryQuota(t, []string{"upstream"}, 1, 1, 1, true)
			svc.SetQuotaManager(qm)
			am := admission.NewManager(newTestLogger(), admission.Options{RInit: 1, RMin: 1, RMax: 1})
			svc.SetAdmissionManager(am)
			state := svc.targetOrder[0]
			state.MarkFailure(time.Now().Add(-time.Minute), time.Second)
			before := state.Stats()
			if _, exceeded := qm.CheckTarget("tester", "upstream", "Executor-Model"); exceeded {
				t.Fatal("fixture: executor group B must remain usable")
			}
			info, exceeded := qm.CheckTarget("tester", "upstream", "Advisor-Model")
			if !exceeded || info.GroupName != "a" {
				t.Fatalf("fixture: group A not exhausted: %+v", info)
			}
			want := httptest.NewRecorder()
			writeQuotaExceededResponse(want, info)
			got := supplementaryRequest(t, svc, "/v1/messages", tc.fields)
			if got.Code != want.Code || got.Body.String() != want.Body.String() {
				t.Errorf("status=%d body=%s, want original quota rejection %s", got.Code, got.Body.String(), want.Body.String())
			}
			if hits.Load() != 0 {
				t.Errorf("rejected request reached upstream %d times", hits.Load())
			}
			if state.inFlightCount() != 0 || state.keyPool.inFlight[0] != 0 || state.keyPool.inFlight[1] != 0 {
				t.Fatal("quota rejection retained node/key occupancy")
			}
			if state.probeSlotBusy(time.Now()) || state.Stats() != before {
				t.Fatal("quota rejection retained probe or changed target health")
			}
			if !am.TryAcquire("upstream") {
				t.Fatal("quota rejection consumed the target's initial rate token")
			}
		})
	}
}

// /v1/messages/count_tokens is an Anthropic request schema, so explicit
// supplementary models are checked at the same target before occupancy.
// Requests without supplementary fields are not extra-checked.
func TestSupplementaryAdmissionAppliesToCountTokens(t *testing.T) {
	for _, tc := range []struct {
		name, fields, model string
		rejected            bool
	}{
		{"explicit_advisor", `,"tools":[{"type":"advisor_20260301","name":"advisor","model":"Advisor-Model"}]`, "Advisor-Model", true},
		{"explicit_fallback", `,"fallbacks":[{"model":"Fallback-Model"}]`, "Fallback-Model", true},
		{"no_supplementary", "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var hits atomic.Int64
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"input_tokens":1}`)
			}))
			defer upstream.Close()
			svc, err := NewService(&config.Config{Targets: []config.Target{{Name: "upstream", EndpointType: "claude", Endpoint: upstream.URL,
				APIKeys: []string{"test-upstream-key", "test-upstream-key-2"}, ModelMappings: []config.ModelMapping{{Upstream: "Executor-Model", Fallback: "executor"}}}}}, newTestLogger())
			if err != nil {
				t.Fatal(err)
			}
			defer svc.Close()
			qm := supplementaryQuota(t, []string{"upstream"}, 1, 1, 1, true)
			svc.SetQuotaManager(qm)
			am := admission.NewManager(newTestLogger(), admission.Options{RInit: 1, RMin: 1, RMax: 1})
			svc.SetAdmissionManager(am)
			state := svc.targetOrder[0]
			before := state.Stats()
			got := supplementaryRequest(t, svc, "/v1/messages/count_tokens", tc.fields)
			if _, exceeded := qm.CheckTarget("tester", "upstream", "Advisor-Model"); !exceeded {
				t.Fatal("fixture: group A must be exhausted")
			}
			if !tc.rejected {
				if got.Code != http.StatusOK || hits.Load() != 1 {
					t.Fatalf("status=%d hits=%d body=%s", got.Code, hits.Load(), got.Body.String())
				}
				return
			}
			info, _ := qm.CheckTarget("tester", "upstream", tc.model)
			want := httptest.NewRecorder()
			writeQuotaExceededResponse(want, info)
			if got.Code != want.Code || got.Body.String() != want.Body.String() {
				t.Fatalf("status=%d body=%s want %s", got.Code, got.Body.String(), want.Body.String())
			}
			if hits.Load() != 0 || state.inFlightCount() != 0 || state.keyPool.inFlight[0] != 0 || state.keyPool.inFlight[1] != 0 {
				t.Fatalf("rejection reached upstream=%d inflight=%d keys=%d/%d", hits.Load(), state.inFlightCount(), state.keyPool.inFlight[0], state.keyPool.inFlight[1])
			}
			if state.probeSlotBusy(time.Now()) || state.Stats() != before {
				t.Fatal("quota rejection retained probe or changed target health")
			}
			if !am.TryAcquire("upstream") {
				t.Fatal("quota rejection consumed the target's initial rate token")
			}
		})
	}
}

func TestSupplementaryAdmissionFinalBody(t *testing.T) {
	advisor := `,"tools":[{"type":"advisor_20260301","name":"advisor","model":"Advisor-Model"}]`
	for _, tc := range []struct {
		name, fields, path, wantModel string
		custom                        map[string]any
		factor                        string
		unspent, reject               bool
	}{
		{name: "both groups usable", fields: advisor, unspent: true, wantModel: "Advisor-Model"},
		{name: "group free", fields: advisor, factor: "group", wantModel: "Advisor-Model"},
		{name: "rank unlimited", fields: advisor, factor: "rank", wantModel: "Advisor-Model"},
		{name: "personal unlimited", fields: advisor, factor: "personal", wantModel: "Advisor-Model"},
		{name: "same group", fields: strings.ReplaceAll(advisor, "Advisor-Model", "Executor-Model"), wantModel: "Executor-Model"},
		{name: "unbound model", fields: strings.ReplaceAll(advisor, "Advisor-Model", "Unbound-Model"), wantModel: "Unbound-Model"},
		{name: "child alias is not rewritten", fields: strings.ReplaceAll(advisor, "Advisor-Model", "advisor-alias"), wantModel: "advisor-alias"},
		{name: "OpenAI custom tools", path: "/v1/chat/completions", fields: advisor + `,"fallback_model":"Fallback-Model"`, wantModel: "Advisor-Model"},
		{name: "ordinary Claude tool", fields: `,"tools":[{"name":"advisor","model":"Advisor-Model","input_schema":{}}]`, wantModel: "Advisor-Model"},
		{name: "arbitrary nested model", fields: `,"metadata":{"model":"Advisor-Model"},"tools":[{"type":"custom","name":"x","input_schema":{"model":"Advisor-Model"}}]`},
		{name: "second advisor checked", fields: `,"tools":[null,{"type":"advisor_20260301","name":"advisor","model":"Executor-Model"},{"type":"advisor_20260301","name":"advisor","model":"Advisor-Model"}]`, reject: true},
		{name: "fallback independently checked", fields: `,"tools":[{"type":"advisor_20260301","name":"advisor","model":"Executor-Model"}],"fallback_model":"Fallback-Model"`, reject: true},
		{name: "custom injects advisor", custom: map[string]any{"tools": json.RawMessage(advisor[len(`,"tools":`):])}, reject: true},
		{name: "custom replaces advisor", fields: advisor, custom: map[string]any{"tools": json.RawMessage(`[{"type":"advisor_20260301","name":"advisor","model":"Executor-Model"}]`)}, wantModel: "Executor-Model"},
		{name: "custom removes advisor", fields: advisor, custom: map[string]any{"tools": nil}},
		{name: "custom injects fallback", custom: map[string]any{"fallback_model": "Fallback-Model"}, reject: true},
		{name: "custom replaces fallback", fields: `,"fallback_model":"Fallback-Model"`, custom: map[string]any{"fallback_model": "Executor-Model"}},
		{name: "duplicate advisor last exhausted", fields: `,"tools":[{"type":"advisor_20260301","name":"advisor","model":"Executor-Model","model":"Advisor-Model"}]`, reject: true},
		{name: "advisor variant does not override", fields: `,"tools":[{"type":"advisor_20260301","name":"advisor","model":"Advisor-Model","Model":"Executor-Model"}]`, reject: true},
		{name: "custom duplicate advisor", custom: map[string]any{"tools": json.RawMessage(`[{"type":"advisor_20260301","name":"advisor","Model":"Executor-Model","model":"Advisor-Model"}]`)}, reject: true},
		{name: "duplicate fallback last exhausted", fields: `,"fallback_model":"Executor-Model","fallback_model":"Fallback-Model"`, reject: true},
		{name: "fallback variant does not override", fields: `,"fallback_model":"Fallback-Model","Fallback_Model":"Executor-Model"`, reject: true},
		{name: "official both groups usable", fields: `,"fallbacks":[{"model":"Unbound-Model"},{"model":"Fallback-Model"}]`, unspent: true},
		{name: "official group free", fields: `,"fallbacks":[{"model":"Fallback-Model"}]`, factor: "group"},
		{name: "official rank unlimited", fields: `,"fallbacks":[{"model":"Fallback-Model"}]`, factor: "rank"},
		{name: "official personal unlimited", fields: `,"fallbacks":[{"model":"Fallback-Model"}]`, factor: "personal"},
		{name: "official all explicit items", fields: `,"fallbacks":[{"model":"Unbound-Model"},{"model":"Another-Unbound"},{"model":"Fallback-Model"}]`, reject: true},
		{name: "official default not inferred", fields: `,"fallbacks":"default"`},
		{name: "official non Anthropic untouched", path: "/v1/chat/completions", fields: `,"fallbacks":[{"model":"Fallback-Model"}]`},
		{name: "official nested model not scanned", fields: `,"metadata":{"fallbacks":[{"model":"Fallback-Model"}]},"fallbacks":[{"model":"Unbound-Model","thinking":{"model":"Fallback-Model"}}]`},
		{name: "official custom injects later item", custom: map[string]any{"fallbacks": json.RawMessage(`[{"model":"Unbound-Model"},{"model":"Fallback-Model"}]`)}, reject: true},
		{name: "official custom overrides allowed value", fields: `,"fallbacks":[{"model":"Unbound-Model"}]`, custom: map[string]any{"fallbacks": json.RawMessage(`[{"model":"Fallback-Model"}]`)}, reject: true},
		{name: "official custom replaces exhausted first", fields: `,"fallbacks":[{"model":"Fallback-Model"},{"model":"Unbound-Model"}]`, custom: map[string]any{"fallbacks": json.RawMessage(`[{"model":"Unbound-Model"}]`)}},
		{name: "official custom replaces exhausted later", fields: `,"fallbacks":[{"model":"Unbound-Model"},{"model":"Fallback-Model"}]`, custom: map[string]any{"fallbacks": json.RawMessage(`[{"model":"Unbound-Model"}]`)}},
		{name: "official custom removes models", fields: `,"fallbacks":[{"model":"Fallback-Model"}]`, custom: map[string]any{"fallbacks": "default"}},
		{name: "official top duplicate last exhausted", fields: `,"fallbacks":[],"fallbacks":[{"model":"Fallback-Model"}]`, reject: true},
		{name: "official top variant does not override", fields: `,"fallbacks":[{"model":"Fallback-Model"}],"Fallbacks":[]`, reject: true},
		{name: "official model duplicate last exhausted", fields: `,"fallbacks":[{"model":"Unbound-Model","model":"Fallback-Model"}]`, reject: true},
		{name: "official model variant does not override", fields: `,"fallbacks":[{"model":"Fallback-Model","Model":"Unbound-Model"}]`, reject: true},
		{name: "official injected model variants", custom: map[string]any{"fallbacks": json.RawMessage(`[{"model":"Fallback-Model","Model":"Unbound-Model"}]`)}, reject: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var captured []byte
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				captured, _ = io.ReadAll(r.Body)
				w.WriteHeader(http.StatusNoContent)
			}))
			defer upstream.Close()
			svc, err := NewService(&config.Config{Targets: []config.Target{{Name: "upstream", EndpointType: "dual_protocol", Endpoint: upstream.URL, APIKey: "test-key", CustomBody: tc.custom,
				ModelMappings: []config.ModelMapping{{Upstream: "Executor-Model", Fallback: "executor"}, {Upstream: "Advisor-Model", Fallback: "advisor-alias"}}}}}, newTestLogger())
			if err != nil {
				t.Fatal(err)
			}
			defer svc.Close()
			group, rank, personal := 1.0, 1.0, 1.0
			switch tc.factor {
			case "group":
				group = 0
			case "rank":
				rank = 0
			case "personal":
				personal = 0
			}
			svc.SetQuotaManager(supplementaryQuota(t, []string{"upstream"}, group, rank, personal, !tc.unspent))
			path := tc.path
			if path == "" {
				path = "/v1/messages"
			}
			got := supplementaryRequest(t, svc, path, tc.fields)
			want := http.StatusNoContent
			if tc.reject {
				want = http.StatusTooManyRequests
			}
			if got.Code != want {
				t.Fatalf("status=%d want=%d body=%s", got.Code, want, got.Body.String())
			}
			if tc.reject && captured != nil {
				t.Fatalf("rejected request reached upstream: %s", captured)
			}
			if !tc.reject && captured == nil {
				t.Fatal("allowed request did not reach upstream")
			}
			if tc.wantModel != "" {
				tool := nestedRaw(t, captured, "tools", "0")
				if got := assertSingleReading(t, tool, "model"); got.value != `"`+tc.wantModel+`"` {
					t.Fatalf("wire advisor model = %s", got.value)
				}
			}
		})
	}
}

func TestSupplementaryAdmissionNormalization(t *testing.T) {
	for _, tc := range []struct {
		name, body, key, value string
		path                   []string
		models                 []string
	}{
		{"fallback duplicate", `{"fallback_model":"blocked","fallback_model":"allowed","Fallback_Model":"blocked"}`, "fallback_model", `"allowed"`, nil, []string{"allowed"}},
		{"fallback variant only", `{"Fallback_Model":"blocked"}`, "fallback_model", "", nil, nil},
		{"fallback invalid last", `{"fallback_model":"blocked","fallback_model":null}`, "fallback_model", "null", nil, nil},
		{"advisor duplicate", `{"tools":[{"type":"advisor_20260301","name":"advisor","model":"blocked","model":"allowed","Model":"blocked"}]}`, "model", `"allowed"`, []string{"tools", "0"}, []string{"allowed"}},
		{"advisor variant only", `{"tools":[{"type":"advisor_20260301","name":"advisor","Model":"blocked"}]}`, "model", "", []string{"tools", "0"}, nil},
		{"advisor invalid last", `{"tools":[{"type":"advisor_20260301","name":"advisor","model":"blocked","model":[]}]}`, "model", "[]", []string{"tools", "0"}, nil},
		{"type duplicate", `{"tools":[{"type":"custom","type":"advisor_20260301","name":"advisor","model":"allowed","Type":"custom"}]}`, "type", `"advisor_20260301"`, []string{"tools", "0"}, []string{"allowed"}},
		{"name duplicate", `{"tools":[{"type":"advisor_20260301","name":"other","name":"advisor","Name":"other","model":"allowed"}]}`, "name", `"advisor"`, []string{"tools", "0"}, []string{"allowed"}},
		{"tools duplicate", `{"tools":[{"type":"advisor_20260301","name":"advisor","model":"blocked"}],"tools":[]}`, "tools", "[]", nil, nil},
		{"escaped model", `{"tools":[{"type":"advisor_20260301","name":"advisor","model":"blocked","\u006dodel":"allowed"}]}`, "model", `"allowed"`, []string{"tools", "0"}, []string{"allowed"}},
		{"official top duplicate", `{"fallbacks":[{"model":"blocked"}],"fallbacks":[{"model":"allowed"}],"Fallbacks":[{"model":"blocked"}]}`, "fallbacks", `[{"model":"allowed"}]`, nil, []string{"allowed"}},
		{"official top variant only", `{"Fallbacks":[{"model":"blocked"}]}`, "fallbacks", "", nil, nil},
		{"official default overrides array", `{"fallbacks":[{"model":"blocked"}],"fallbacks":"default"}`, "fallbacks", `"default"`, nil, nil},
		{"official model duplicate", `{"fallbacks":[{"model":"blocked","model":"allowed","Model":"blocked"}]}`, "model", `"allowed"`, []string{"fallbacks", "0"}, []string{"allowed"}},
		{"official later model duplicate", `{"fallbacks":[{"model":"first"},{"model":"blocked","model":"allowed","Model":"blocked"}]}`, "model", `"allowed"`, []string{"fallbacks", "1"}, []string{"first", "allowed"}},
		{"official model variant only", `{"fallbacks":[{"Model":"blocked"}]}`, "model", "", []string{"fallbacks", "0"}, nil},
		{"official invalid last model", `{"fallbacks":[{"model":"blocked","model":null}]}`, "model", "null", []string{"fallbacks", "0"}, nil},
		{"official escaped model", `{"fallbacks":[{"model":"blocked","\u006dodel":"allowed"}]}`, "model", `"allowed"`, []string{"fallbacks", "0"}, []string{"allowed"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, models, report := prepareSupplementaryAdmission("/v1/messages", []byte(tc.body))
			if len(report) == 0 || !reflect.DeepEqual(models, tc.models) {
				t.Fatalf("models=%v report=%v", models, report)
			}
			if got := assertSingleReading(t, nestedRaw(t, body, tc.path...), tc.key); got.value != tc.value {
				t.Fatalf("value=%s want=%s body=%s", got.value, tc.value, body)
			}
			again, againModels, againReport := prepareSupplementaryAdmission("/v1/messages", body)
			if !bytes.Equal(again, body) || len(againReport) != 0 || !reflect.DeepEqual(againModels, models) {
				t.Fatal("normalization is not idempotent")
			}
		})
	}
	for _, body := range []string{`{`, `null`, `{}`, `{"tools":{}}`, `{"tools":[null,7,"model",[]]}`, `{"fallback_model":3}`, `{"fallback_model":{}}`,
		`{"tools":[{"type":"advisor_20260301","name":"advisor","model":null}]}`,
		`{"tools":[{"type":"advisor_20260301","name":"advisor"}]}`,
		`{"tools":[{"type":"advisor_20260301","name":"advisor","model":{"model":"blocked"}}]}`,
		`{"tools":[{"type":"custom","name":"advisor","model":"a","model":"b"}]}`,
		`{"fallbacks":"default"}`, `{"fallbacks":[null,3,"blocked",[],{}, {"model":null},{"model":""},{"model":{"model":"blocked"}}]}`, `{"fallbacks":{"model":"blocked"}}`} {
		got, models, report := prepareSupplementaryAdmission("/v1/messages", []byte(body))
		if string(got) != body || len(models) != 0 || len(report) != 0 {
			t.Fatalf("invalid/unrelated body changed or checked: %s -> %s, %v", body, got, models)
		}
	}
}

func TestSupplementaryAdmissionFailover(t *testing.T) {
	advisor := json.RawMessage(`[{"type":"advisor_20260301","name":"advisor","model":"Advisor-Model"}]`)
	for _, tc := range []struct {
		name, fields  string
		first, second map[string]any
		allowed       bool
	}{
		{name: "actual target group", fields: `,"tools":` + string(advisor)},
		{name: "original body restored", fields: `,"tools":` + string(advisor), first: map[string]any{"tools": nil}},
		{name: "second target injects", second: map[string]any{"tools": advisor}},
		{name: "second target removes", fields: `,"tools":` + string(advisor), second: map[string]any{"tools": nil}, allowed: true},
		{name: "fallback actual target", fields: `,"fallback_model":"Fallback-Model"`},
		{name: "fallback original restored", fields: `,"fallback_model":"Fallback-Model"`, first: map[string]any{"fallback_model": "Executor-Model"}},
		{name: "official actual target", fields: `,"fallbacks":[{"model":"Unbound-Model"},{"model":"Fallback-Model"}]`},
		{name: "official original restored", fields: `,"fallbacks":[{"model":"Unbound-Model"},{"model":"Fallback-Model"}]`, first: map[string]any{"fallbacks": "default"}},
		{name: "official second target injects", second: map[string]any{"fallbacks": json.RawMessage(`[{"model":"Unbound-Model"},{"model":"Fallback-Model"}]`)}},
		{name: "official second target overrides", fields: `,"fallbacks":[{"model":"Fallback-Model"}]`, second: map[string]any{"fallbacks": json.RawMessage(`[{"model":"Unbound-Model"}]`)}, allowed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var hits atomic.Int64
			second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				w.WriteHeader(http.StatusNoContent)
			}))
			defer second.Close()
			cfg := &config.Config{Targets: []config.Target{
				{Name: "first", EndpointType: "claude", Endpoint: "http://first.invalid", APIKeys: []string{"first-key-1", "first-key-2"}, CustomBody: tc.first, ModelMappings: []config.ModelMapping{{Upstream: "Executor-First", Fallback: "executor"}}},
				{Name: "second", EndpointType: "claude", Endpoint: second.URL, APIKeys: []string{"second-key-1", "second-key-2"}, CustomBody: tc.second, ModelMappings: []config.ModelMapping{{Upstream: "Executor-Model", Fallback: "executor"}}},
			}}
			svc, err := NewService(cfg, newTestLogger())
			if err != nil {
				t.Fatal(err)
			}
			defer svc.Close()
			secondURL, _ := url.Parse(second.URL)
			svc.httpClient = &http.Client{Transport: &failingTransport{successHost: secondURL.Host}}
			svc.SetQuotaManager(supplementaryQuota(t, []string{"second"}, 1, 1, 1, true))
			svc.affinity.Set(sessionAffinityKey("tester", "admission-session", "executor"), "first", time.Now())
			r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"executor","messages":[]`+tc.fields+`}`))
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("X-Session-Affinity", "admission-session")
			r = r.WithContext(auth.WithPrincipal(r.Context(), newAllowAllPrincipal(t, "test-key")))
			got := httptest.NewRecorder()
			svc.ServeHTTP(got, r)
			want, wantHits := http.StatusTooManyRequests, int64(0)
			if tc.allowed {
				want, wantHits = http.StatusNoContent, 1
			}
			if got.Code != want || hits.Load() != wantHits || svc.MetricsSnapshot().TotalTargetRetries != 1 {
				t.Fatalf("status=%d body=%s hits=%d retries=%d", got.Code, got.Body.String(), hits.Load(), svc.MetricsSnapshot().TotalTargetRetries)
			}
			for _, state := range svc.targetOrder {
				if state.inFlightCount() != 0 || state.keyPool.inFlight[0] != 0 || state.keyPool.inFlight[1] != 0 {
					t.Fatal("failover retained node/key occupancy")
				}
			}
		})
	}
}

func TestSupplementaryAdmissionInvalidFieldsRemainUpstreamValidated(t *testing.T) {
	var captured []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"type":"invalid_request_error","message":"invalid supplementary field"}}`)
	}))
	defer upstream.Close()
	svc, err := NewService(&config.Config{Targets: []config.Target{{Name: "upstream", EndpointType: "claude", Endpoint: upstream.URL, APIKey: "test-key",
		ModelMappings: []config.ModelMapping{{Upstream: "executor"}}}}}, newTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	svc.SetQuotaManager(supplementaryQuota(t, []string{"upstream"}, 1, 1, 1, true))
	for _, fields := range []string{`,"fallback_model":null`, `,"fallback_model":["Fallback-Model"]`, `,"tools":[{"type":"advisor_20260301","name":"advisor","model":7}]`, `,"tools":[{"type":"advisor_20260301","name":"advisor"}]`,
		`,"fallbacks":[{"model":7}]`, `,"fallbacks":[{"model":["Fallback-Model"]}]`, `,"fallbacks":[{}]`, `,"fallbacks":{"model":"Fallback-Model"}`} {
		got := supplementaryRequest(t, svc, "/v1/messages", fields)
		if got.Code != http.StatusBadRequest || !strings.Contains(got.Body.String(), "invalid supplementary field") || !bytes.Contains(captured, []byte(fields[1:])) {
			t.Fatalf("invalid field not passed to upstream validation: %s status=%d captured=%s", fields, got.Code, captured)
		}
	}
}

func FuzzSupplementaryAdmission(f *testing.F) {
	for _, body := range []string{`{}`, `{"fallback_model":"a","Fallback_Model":"b"}`, `{"tools":[null,{},[],{"type":"advisor_20260301","name":"advisor","model":"a","model":null}]}`,
		`{"fallbacks":[{"model":"a"},{"Model":"b","model":"c","model":"d"}]}`, `{"fallbacks":[],"fallbacks":"default"}`} {
		f.Add(body)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		body := []byte(raw)
		got, models, report := prepareSupplementaryAdmission("/v1/messages", body)
		if len(report) == 0 {
			if !bytes.Equal(body, got) {
				t.Fatal("changed body without normalization")
			}
		} else if !json.Valid(got) {
			t.Fatalf("normalization produced invalid JSON: %q -> %q", body, got)
		}
		again, againModels, againReport := prepareSupplementaryAdmission("/v1/messages", got)
		if !bytes.Equal(got, again) || !reflect.DeepEqual(models, againModels) || len(againReport) != 0 {
			t.Fatal("normalization not idempotent")
		}
	})
}
