package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/nosql"
	"github.com/LeXwDeX/LLM-Proxy/internal/pricing"
	"github.com/LeXwDeX/LLM-Proxy/internal/quota"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

func TestGPTBillingThroughProxy(t *testing.T) {
	for _, provider := range []string{"openai", "azure_openai"} {
		for _, responses := range []bool{false, true} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/responses=%t/stream=%t", provider, responses, stream), func(t *testing.T) {
					path := "/v1/chat/completions"
					usageJSON := `{"prompt_tokens":15000,"completion_tokens":300,"total_tokens":15300,"prompt_tokens_details":{"cached_tokens":12000,"cache_write_tokens":2000},"completion_tokens_details":{"reasoning_tokens":200,"accepted_prediction_tokens":20,"rejected_prediction_tokens":10}}`
					if responses {
						path = "/v1/responses"
						usageJSON = `{"input_tokens":15000,"output_tokens":300,"total_tokens":15300,"input_tokens_details":{"cached_tokens":12000,"cache_write_tokens":2000},"output_tokens_details":{"reasoning_tokens":200}}`
					}
					payload := `{"model":"gpt-5.6-sol","usage":` + usageJSON + `}`
					wire := payload
					contentType := "application/json"
					if stream {
						contentType = "text/event-stream"
						if responses {
							payload = `{"type":"response.completed","response":` + payload + `}`
						}
						// A preliminary snapshot must not inflate ordinary input, and repeated
						// final snapshots must not add the same charge twice.
						wire = "data: {\"model\":\"gpt-5.6-sol\",\"usage\":{\"input_tokens\":15000,\"output_tokens\":0}}\n\n" +
							strings.Repeat("data: {\"type\":\"output.delta\",\"delta\":\"x\"}\n\n", 15) +
							"data: " + payload + "\n\ndata: " + payload + "\n\n" +
							strings.Repeat("data: {\"type\":\"tool.done\"}\n\n", 15) + "data: [DONE]\n\n"
					}
					upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						w.Header().Set("Content-Type", contentType)
						_, _ = w.Write([]byte(wire))
					}))
					defer upstream.Close()
					db, err := nosql.OpenDB(filepath.Join(t.TempDir(), "usage.db"))
					if err != nil {
						t.Fatal(err)
					}
					defer db.Close()
					store := nosql.NewUsageStore(db)
					clients := nosql.NewClientStore(db)
					// 额度 v2：客户端必须绑定职级才有额度（未绑 = 拒绝）。
					if err := clients.Create(config.Client{Name: "tester", AccessKey: "token", JobLevel: "L5"}); err != nil {
						t.Fatal(err)
					}
					if err := nosql.NewJobLevelStore(db).Create(config.JobLevel{Name: "L5"}); err != nil {
						t.Fatal(err)
					}
					rates := usage.CostRates{InputPer1MTokens: 4, OutputPer1MTokens: 20, CacheReadPer1MToken: 0.4, CacheWritePer1MToken: 5}
					templates := []pricing.PricingTemplate{{Target: "upstream", Model: "gpt-5.6-sol", BaseRates: rates}}
					cache := pricing.NewPricingCache(func() ([]pricing.PricingTemplate, error) { return templates, nil }, nil)
					qm, err := quota.New(quota.Options{CostCache: cache, UsageStore: store, ClientStore: clients, JobLevelStore: nosql.NewJobLevelStore(db), Logger: newTestLogger()})
					if err != nil {
						t.Fatal(err)
					}
					qm.Evaluate("tester")
					cfg := &config.Config{Server: config.ServerConfig{RequestTimeoutSeconds: 5}, Targets: []config.Target{{Name: "upstream", Endpoint: upstream.URL, EndpointType: provider, APIKey: "key"}}}
					cfg.TraceStore.Enabled = true
					cfg.TraceStore.DiskPath = filepath.Join(t.TempDir(), "trace.log")
					cfg.TraceStore.MaxBodySize = 64 // Diagnostic body truncation must not truncate token metadata.
					svc, err := NewService(cfg, newTestLogger())
					if err != nil {
						t.Fatal(err)
					}
					defer svc.Close()
					svc.SetUsageRecorder(store)
					svc.SetQuotaManager(qm)
					authStore := auth.NewStore()
					if err := authStore.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
						t.Fatal(err)
					}
					principal, _ := authStore.Authenticate("token")
					body := fmt.Sprintf(`{"model":"gpt-5.6-sol","messages":[{"role":"user","content":"hi"}],"stream":%t}`, stream)
					if responses {
						body = fmt.Sprintf(`{"model":"gpt-5.6-sol","input":"hi","stream":%t}`, stream)
					}
					req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
					req.Header.Set("Content-Type", "application/json")
					req = req.WithContext(auth.WithPrincipal(req.Context(), principal))
					rr := httptest.NewRecorder()
					svc.ServeHTTP(rr, req)
					if rr.Code != 200 || rr.Body.String() != wire {
						t.Fatalf("response changed: status=%d body=%s", rr.Code, rr.Body.String())
					}
					events, err := store.List(usage.Filter{Limit: 10})
					if err != nil {
						t.Fatal(err)
					}
					if len(events) != 1 {
						t.Fatalf("events=%+v", events)
					}
					e := events[0]
					if e.EndpointType != provider || e.Model != "gpt-5.6-sol" || e.InputTokens != 1000 || e.OutputTokens != 300 || e.CachedTokens != 12000 || e.CacheCreationTokens != 2000 {
						t.Fatalf("wrong partition: %+v", e)
					}
					const wantCost = 0.0248
					if got := usage.EstimateEventCost(e, cache.Get()); math.Abs(got-wantCost) > 1e-10 {
						t.Fatalf("cost=%g want %g", got, wantCost)
					}
					from := e.Timestamp.Truncate(time.Hour)
					to := from.Add(time.Hour)
					agg, err := store.AggregateByModelGroupsAllRequests(usage.Filter{From: &from, To: &to}, "hour", cache.Get(), nosql.NewTargetStore(db))
					if err != nil {
						t.Fatal(err)
					}
					if agg.Totals.CacheCreationTokens != 2000 || math.Abs(agg.Totals.EstimatedCost-wantCost) > 1e-10 {
						t.Fatalf("aggregate=%+v", agg.Totals)
					}
					checkQuota := func(stage string) {
						t.Helper()
						// tester 无 plan → 全部流量归 company_general（系数 1，折算=原价）。
						got := qm.Status("tester").Groups["company_general"].Quotas["daily"].Used
						if math.IsNaN(got) || math.Abs(got-wantCost) > 1e-10 {
							t.Fatalf("%s quota=%g want %g", stage, got, wantCost)
						}
					}
					checkQuota("incremental")
					qm.Evaluate("tester")
					checkQuota("recalibrated")
					checkTrace := func() {
						t.Helper()
						trace := svc.GetTrace(e.RequestID)
						if trace == nil {
							t.Fatal("missing request trace")
						}
						data, err := json.Marshal(trace)
						if err != nil {
							t.Fatal(err)
						}
						var fields map[string]any
						if err := json.Unmarshal(data, &fields); err != nil {
							t.Fatal(err)
						}
						for key, want := range map[string]int64{
							"input_tokens": e.InputTokens, "output_tokens": e.OutputTokens,
							"cached_tokens": e.CachedTokens, "cache_creation_tokens": e.CacheCreationTokens,
						} {
							if got := fields[key]; got != float64(want) {
								t.Errorf("trace %s=%v want billing usage %d", key, got, want)
							}
						}
					}
					checkTrace()
					// Trace collection must also work when billing storage is disabled.
					svc.SetUsageRecorder(nil)
					req = httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)).WithContext(req.Context())
					req.Header.Set("Content-Type", "application/json")
					rr = httptest.NewRecorder()
					svc.ServeHTTP(rr, req)
					if rr.Code != 200 || rr.Body.String() != wire {
						t.Fatalf("response without billing recorder changed: status=%d", rr.Code)
					}
					checkTrace()
				})
			}
		}
	}
}

func TestGPTUsageScopeAndMissingFields(t *testing.T) {
	cases := []struct {
		name, provider, model, body string
		want                        usageTokens
		found                       bool
	}{
		{"mixed-case", "openai", "GPT-5.6-SOL", `{"usage":{"prompt_tokens":100,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":30,"cache_write_tokens":50}}}`, usageTokens{InputTokens: 20, OutputTokens: 20, CachedTokens: 30, CacheCreationTokens: 50}, true},
		{"old-gpt-no-write", "azure_openai", "gpt-4o", `{"usage":{"prompt_tokens":100,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":30}}}`, usageTokens{InputTokens: 70, OutputTokens: 20, CachedTokens: 30, CacheCreationTokens: 0}, true},
		{"no-cache", "openai", "gpt-5.6-sol", `{"usage":{"input_tokens":100,"output_tokens":20}}`, usageTokens{InputTokens: 100, OutputTokens: 20, CachedTokens: 0, CacheCreationTokens: 0}, true},
		{"all-written", "openai", "gpt-5.6-sol", `{"usage":{"input_tokens":100,"output_tokens":20,"input_tokens_details":{"cache_write_tokens":100}}}`, usageTokens{InputTokens: 0, OutputTokens: 20, CachedTokens: 0, CacheCreationTokens: 100}, true},
		{"all-read", "azure_openai", "gpt-5.6-sol", `{"usage":{"input_tokens":100,"output_tokens":20,"input_tokens_details":{"cached_tokens":100,"cache_write_tokens":0}}}`, usageTokens{InputTokens: 0, OutputTokens: 20, CachedTokens: 100, CacheCreationTokens: 0}, true},
		{"invalid-counts", "openai", "gpt-5.6-sol", `{"usage":{"input_tokens":100,"output_tokens":-1,"input_tokens_details":{"cached_tokens":150,"cache_write_tokens":-20}}}`, usageTokens{InputTokens: 0, OutputTokens: 0, CachedTokens: 150, CacheCreationTokens: 0}, true},
		{"response-model-fallback", "openai", "", `{"model":"GPT-5.6-SOL","usage":{"input_tokens":100,"output_tokens":20,"input_tokens_details":{"cache_write_tokens":50}}}`, usageTokens{InputTokens: 50, OutputTokens: 20, CachedTokens: 0, CacheCreationTokens: 50}, true},
		{"mapped-model-wins", "openai", "other-model", `{"model":"gpt-5.6-sol","usage":{"input_tokens":100,"output_tokens":20,"input_tokens_details":{"cache_write_tokens":50}}}`, usageTokens{InputTokens: 50, OutputTokens: 20, CachedTokens: 0, CacheCreationTokens: 50}, true},
		{"other-provider", "dual_protocol", "gpt-5.6-sol", `{"usage":{"input_tokens":100,"output_tokens":20,"input_tokens_details":{"cache_write_tokens":50}}}`, usageTokens{InputTokens: 50, OutputTokens: 20, CachedTokens: 0, CacheCreationTokens: 50}, true},
		{"other-model", "openai", "o3", `{"usage":{"input_tokens":100,"output_tokens":20,"input_tokens_details":{"cache_write_tokens":50}}}`, usageTokens{InputTokens: 50, OutputTokens: 20, CachedTokens: 0, CacheCreationTokens: 50}, true},
		{"no-usage", "openai", "gpt-5.6-sol", `{"usage":null}`, usageTokens{}, false},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got, _, found := (usageParser{endpointType: tt.provider, model: tt.model}).extractTokens("application/json", []byte(tt.body))
			if got.InputTokens != tt.want.InputTokens || got.OutputTokens != tt.want.OutputTokens || got.CachedTokens != tt.want.CachedTokens || got.CacheCreationTokens != tt.want.CacheCreationTokens || found != tt.found {
				t.Fatalf("got %+v %t want %+v %t", got, found, tt.want, tt.found)
			}
		})
	}
}

func TestAzureGPTCacheControls(t *testing.T) {
	for _, path := range []string{"/openai/v1/chat/completions", "/openai/v1/responses"} {
		body := []byte(`{"model":"gpt-5.6-sol","prompt_cache_options":{"mode":"explicit","ttl":"30m"},"prompt_cache_retention":"24h","unrecognized":true}`)
		got, stripped := sanitizeRequestBodyForAzure(httptest.NewRequest(http.MethodPost, path, nil), body)
		if len(stripped) != 1 || stripped[0] != "unrecognized" || !bytes.Contains(got, []byte(`"prompt_cache_options"`)) || !bytes.Contains(got, []byte(`"prompt_cache_retention"`)) {
			t.Fatalf("got %s stripped %v", got, stripped)
		}
	}
}
