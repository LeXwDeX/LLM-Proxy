package proxy

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/nosql"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

type failFirstUsageWrite struct {
	next usage.Recorder
	err  error
	done bool
}

func (r *failFirstUsageWrite) Record(event usage.Event) error {
	if !r.done {
		r.done = true
		return r.err
	}
	return r.next.Record(event)
}

type alwaysFailUsageWrite struct{ err error }

func (r alwaysFailUsageWrite) Record(usage.Event) error { return r.err }

func TestPermanentUsageRecordFailureIsObservable(t *testing.T) {
	service, err := NewService(&config.Config{}, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	service.SetUsageRecorder(alwaysFailUsageWrite{err: errors.New("database and journal unavailable")})

	principal := newAllowAllPrincipal(t, "usage-failure-token")
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	request = request.WithContext(auth.WithPrincipal(request.Context(), principal))
	service.recordParsedUsage(
		request,
		&Target{Name: "failed-target"},
		http.StatusOK,
		"qwen-max",
		usageTokens{InputTokens: 11},
		"qwen-max",
		nil,
		-1,
	)

	if got := service.MetricsSnapshot().TotalUsageRecordFailures; got != 1 {
		t.Fatalf("usage record failure metric = %d, want 1", got)
	}
}

func TestServiceDefersSuccessfulResponseUsageUntilRecorderRecovers(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"qwen-max","usage":{"prompt_tokens":11,"completion_tokens":7}}`))
	}))
	defer upstream.Close()

	tmpDir := t.TempDir()
	db, err := nosql.OpenDB(filepath.Join(tmpDir, "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	usageStore := nosql.NewUsageStore(db)
	primary := &failFirstUsageWrite{next: usageStore, err: errors.New("temporary usage write failure")}
	journal, err := nosql.NewUsageJournal(primary, filepath.Join(tmpDir, "usage-journal.jsonl"), newTestLogger())
	if err != nil {
		t.Fatalf("NewUsageJournal: %v", err)
	}

	cfg := &config.Config{
		Server: config.ServerConfig{RequestTimeoutSeconds: 5},
		Targets: []config.Target{{
			Name:               "journal-target",
			Endpoint:           upstream.URL,
			ResourcePathPrefix: "/openai",
			APIKey:             "upstream-fixture",
		}},
	}
	service, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	service.SetUsageRecorder(journal)

	principal := newAllowAllPrincipal(t, "journal-token")
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"qwen-max"}`))
	request = request.WithContext(auth.WithPrincipal(request.Context(), principal))
	response := httptest.NewRecorder()
	service.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("successful upstream response changed to HTTP %d: %s", response.Code, response.Body.String())
	}
	if !bytes.Contains(response.Body.Bytes(), []byte(`"completion_tokens":7`)) {
		t.Fatalf("upstream response body was not delivered: %s", response.Body.String())
	}

	items, err := usageStore.List(usage.Filter{ClientName: "tester"})
	if err != nil {
		t.Fatalf("List before replay: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("failed primary write unexpectedly appeared in store: %d events", len(items))
	}

	recoveredClients, err := journal.Replay()
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(recoveredClients) != 1 || recoveredClients[0] != "tester" {
		t.Fatalf("Replay recovered clients %v, want [tester]", recoveredClients)
	}
	items, err = usageStore.List(usage.Filter{ClientName: "tester"})
	if err != nil {
		t.Fatalf("List after replay: %v", err)
	}
	if len(items) != 1 || items[0].InputTokens != 11 || items[0].OutputTokens != 7 {
		t.Fatalf("recovered response usage mismatch: %+v", items)
	}
}
