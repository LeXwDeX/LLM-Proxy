package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/nosql"
	"github.com/LeXwDeX/LLM-Proxy/internal/proxy"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	"github.com/go-chi/chi/v5"
	bolt "go.etcd.io/bbolt"
)

func TestActivityReadonlyEndpoint(t *testing.T) {
	entry, _, _ := newNamedTargetUpdateTestHandler(t, t.TempDir(), "local", []string{"test-local-key"})
	h := entry.(*Handler)
	sm := NewSessionManager(config.AdminSessionConfig{Secret: "activity-local-secret", CookieName: "activity-session", TTLSeconds: 3600}, h.logger)
	h.sessionManager = sm
	h.router = h.routes()
	router := chi.NewRouter()
	router.Use(sm.Middleware)
	router.Mount("/admin", h)
	_, cookie, err := sm.CreateSession("readonly", RoleReadonly)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/admin/api/activity", nil)
	req.AddCookie(cookie)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, req)
	if response.Code != http.StatusOK {
		t.Fatalf("authenticated activity endpoint: got %d want 200: %s", response.Code, response.Body.String())
	}
	anonymous := httptest.NewRecorder()
	router.ServeHTTP(anonymous, httptest.NewRequest(http.MethodGet, "/admin/api/activity", nil))
	if anonymous.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous activity API: got %d want 401", anonymous.Code)
	}
}

func TestActivityClientUnionTokenTotalsAndPrivacy(t *testing.T) {
	entry, stores, _ := newNamedTargetUpdateTestHandler(t, t.TempDir(), "local", []string{"never-output-target-secret"})
	h := entry.(*Handler)
	for _, name := range []string{"Alice", "Deleted"} {
		if err := stores.clientStore.Create(config.Client{Name: name, AccessKey: "never-output-client-secret-" + name}); err != nil {
			t.Fatal(err)
		}
	}
	if err := stores.clientStore.Delete("Deleted"); err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Add(-time.Minute)
	for _, evt := range []usage.Event{
		{Timestamp: at, ClientName: "ALICE", InputTokens: 1, CachedTokens: 2, CacheCreationTokens: 3, ToolUsePromptTokens: 4, OutputTokens: 5, ReasoningTokens: 99, ImageInputTokens: 999, CachedImageInputTokens: 999, ImageOutputTokens: 999, ExplicitCacheReadTokens: 99, CacheCreation5mTokens: 99, CacheCreation1hTokens: 99, UsageStatus: usage.UsageStatusIncomplete},
		{Timestamp: at, ClientName: "alice", InputTokens: 2, OutputTokens: 3},
		{Timestamp: at, ClientName: "Deleted", InputTokens: 7, OutputTokens: 11},
		{Timestamp: at, ClientName: "Unknown", InputTokens: 13, OutputTokens: 17},
	} {
		if err := stores.usageStore.Record(evt); err != nil {
			t.Fatal(err)
		}
	}
	response := httptest.NewRecorder()
	h.handleActivity(response, httptest.NewRequest(http.MethodGet, "/api/activity", nil))
	if response.Code != 200 {
		t.Fatalf("activity: %d %s", response.Code, response.Body.String())
	}
	for _, secret := range []string{"never-output", "access_key", "api_key", "allowed_targets", "estimated_cost"} {
		if strings.Contains(response.Body.String(), secret) {
			t.Fatalf("activity exposes sensitive or unrelated field %s", secret)
		}
	}
	var body activityResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Totals != (activityTotals{InputTokens: 32, OutputTokens: 36, TotalTokens: 68, IncompleteUsageCount: 1}) {
		t.Fatalf("token subsets were counted twice: %+v", body.Totals)
	}
	if !body.GeneratedAt.Equal(body.Window.To) || body.Window.To.Sub(body.Window.From) != 5*time.Minute || body.Window.StepSeconds != 10 || body.Runtime.Complete || len(body.Series) < 30 || len(body.Series) > 31 {
		t.Fatalf("window or cold runtime contract: %+v", body.Window)
	}
	if body.Runtime.ActiveRequests != 0 || body.Runtime.ActiveClients != 0 || body.Runtime.WebsocketSessions != 0 {
		t.Fatal("cold runtime counts are not zero")
	}
	found := map[string]activityClient{}
	for _, client := range body.Clients {
		found[client.Name] = client
		if len(client.Series) != len(body.Series) {
			t.Fatal("client/global buckets differ")
		}
		for i, bucket := range client.Series {
			if !bucket.From.Equal(body.Series[i].From) || !bucket.To.Equal(body.Series[i].To) {
				t.Fatal("bucket alignment differs")
			}
		}
	}
	if len(found) != 4 || !found["Alice"].Configured || found["Alice"].Totals.TotalTokens != 20 || found["Deleted"].Configured || found["Deleted"].Totals.TotalTokens != 18 || found["Unknown"].Configured || found["Unknown"].Totals.TotalTokens != 30 {
		t.Fatalf("case merging or deleted/unknown retention: %+v", found)
	}
	if !found["client-1"].Configured || found["client-1"].Totals.TotalTokens != 0 {
		t.Fatal("configured idle client not included with zero usage")
	}
	var total int64
	for _, bucket := range body.Series {
		total += bucket.TotalTokens
	}
	if total != 68 {
		t.Fatal("global series differs from window totals")
	}
	if body.Series[0].ActiveRequestsPeak != nil {
		t.Fatal("pre-start runtime history must be null")
	}
}

func TestActivityStorageAndCancellationFailures(t *testing.T) {
	for _, mode := range []string{"canceled", "corrupt", "closed"} {
		t.Run(mode, func(t *testing.T) {
			entry, stores, _ := newNamedTargetUpdateTestHandler(t, t.TempDir(), "local", []string{"local-test"})
			h := entry.(*Handler)
			req := httptest.NewRequest(http.MethodGet, "/api/activity", nil)
			switch mode {
			case "canceled":
				ctx, cancel := context.WithCancel(req.Context())
				cancel()
				req = req.WithContext(ctx)
			case "corrupt":
				if err := stores.clientStore.DB().Update(func(tx *bolt.Tx) error {
					return tx.Bucket([]byte(nosql.BucketUsageEvents)).Put([]byte(time.Now().UTC().Add(-time.Second).Format(time.RFC3339Nano)+"_bad"), []byte("credential-sensitive-bad-json"))
				}); err != nil {
					t.Fatal(err)
				}
			case "closed":
				if err := stores.clientStore.DB().Close(); err != nil {
					t.Fatal(err)
				}
			}
			response := httptest.NewRecorder()
			h.handleActivity(response, req)
			if response.Code != 500 || strings.TrimSpace(response.Body.String()) != `{"error":"internal server error"}` {
				t.Fatalf("failed activity read must not return zero/partial payload: %d %s", response.Code, response.Body.String())
			}
		})
	}
}

func BenchmarkActivitySerialization300Clients31Buckets(b *testing.B) {
	to := time.Date(2026, 10, 14, 13, 0, 1, 0, time.UTC)
	runtime := proxy.ActivitySnapshot{From: to.Add(-5 * time.Minute), To: to}
	for at := runtime.From.Truncate(proxy.ActivityStep); at.Before(to); at = at.Add(proxy.ActivityStep) {
		end := at.Add(proxy.ActivityStep)
		if end.After(to) {
			end = to
		}
		start := at
		if start.Before(runtime.From) {
			start = runtime.From
		}
		runtime.Intervals = append(runtime.Intervals, proxy.ActivityInterval{From: start, To: end, Partial: end.Sub(start) != proxy.ActivityStep})
		zero := 0
		runtime.Peaks = append(runtime.Peaks, &zero)
	}
	response := activityResponse{GeneratedAt: to, Series: buildActivitySeries(runtime, runtime.Peaks, make([]usage.Totals, 31))}
	for i := 0; i < 300; i++ {
		response.Clients = append(response.Clients, activityClient{Name: "synthetic-client", Configured: true, Series: buildActivitySeries(runtime, runtime.Peaks, make([]usage.Totals, 31))})
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := json.Marshal(response); err != nil {
			b.Fatal(err)
		}
	}
}
