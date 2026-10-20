package proxy

import (
	"io"
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
	bolt "go.etcd.io/bbolt"
)

type accountingReviewTransport func(*http.Request) (*http.Response, error)

func (f accountingReviewTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestCoreReviewAccountingUnavailableReturns503UntilRawHistoryRecovers(t *testing.T) {
	db, err := nosql.OpenDB(filepath.Join(t.TempDir(), "accounting.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	clients := nosql.NewClientStore(db)
	levels := nosql.NewJobLevelStore(db)
	events := nosql.NewUsageStore(db)
	if err := levels.Create(config.JobLevel{Name: "employee"}); err != nil {
		t.Fatal(err)
	}
	if err := clients.Create(config.Client{Name: "tester", AccessKey: "fixture", JobLevel: "employee"}); err != nil {
		t.Fatal(err)
	}
	cache := pricing.NewPricingCache(func() ([]pricing.PricingTemplate, error) {
		return []pricing.PricingTemplate{{Model: "test", Mode: pricing.ModeSchedule, Timezone: "+00:00", BaseRates: usage.CostRates{InputPer1MTokens: 1}, Periods: []pricing.PricingPeriod{{Weekdays: []int{1, 2, 3, 4, 5, 6, 7}, Start: "00:30", End: "24:00", Rates: usage.CostRates{InputPer1MTokens: 1}}}}}, nil
	}, nil)
	manager, err := quota.New(quota.Options{ClientStore: clients, JobLevelStore: levels, UsageStore: events, CostCache: cache, Logger: newTestLogger()})
	if err != nil {
		t.Fatal(err)
	}
	if err := events.Record(usage.Event{Timestamp: time.Now().UTC(), ClientName: "tester", Target: "test-target", Model: "test", InputTokens: 1, StatusCode: 200}); err != nil {
		t.Fatal(err)
	}
	var key, data []byte
	if err := db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte(nosql.BucketUsageEvents))
		k, v := bucket.Cursor().First()
		key = append([]byte(nil), k...)
		data = append([]byte(nil), v...)
		return bucket.Delete(k)
	}); err != nil {
		t.Fatal(err)
	}
	manager.Evaluate("tester")
	if manager.Status("tester").CalibrationError == "" {
		t.Fatal("missing raw history did not block calibration")
	}
	svc := newTestModelService(t, config.EndpointTypeOpenAI, "https://fixture.invalid", func(target *config.Target) {
		target.ModelMappings = []config.ModelMapping{{Upstream: "test"}}
	})
	defer svc.Close()
	svc.SetQuotaManager(manager)
	calls := 0
	svc.httpClient.Transport = accountingReviewTransport(func(req *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"usage":{"prompt_tokens":1,"completion_tokens":1}}`)), Request: req}, nil
	})
	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "fixture")); err != nil {
		t.Fatal(err)
	}
	principal, _ := store.Authenticate("fixture")
	request := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"test"}`))
		r = r.WithContext(auth.WithPrincipal(r.Context(), principal))
		w := httptest.NewRecorder()
		svc.ServeHTTP(w, r)
		return w
	}
	blocked := request()
	if blocked.Code != 503 || calls != 0 || !strings.Contains(blocked.Body.String(), "accounting_unavailable") {
		t.Fatalf("accounting status=%d upstream calls=%d body=%s", blocked.Code, calls, blocked.Body.String())
	}
	if strings.Contains(blocked.Body.String(), "resets_at") {
		t.Fatal("accounting failure must not claim a quota reset time")
	}
	if err := db.Update(func(tx *bolt.Tx) error { return tx.Bucket([]byte(nosql.BucketUsageEvents)).Put(key, data) }); err != nil {
		t.Fatal(err)
	}
	manager.Evaluate("tester")
	if manager.Status("tester").CalibrationError != "" {
		t.Fatal("raw history recovery did not clear accounting failure")
	}
	recovered := request()
	if recovered.Code != 200 || calls != 1 {
		t.Fatalf("recovered status=%d upstream calls=%d", recovered.Code, calls)
	}
}
