package admin

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/nosql"
	"github.com/LeXwDeX/LLM-Proxy/internal/proxy"
	"github.com/LeXwDeX/LLM-Proxy/internal/tracestore"
)

type issue110AuditResponse struct {
	Events []nosql.AuditEvent `json:"events"`
	Count  int                `json:"count"`
}

func issue110AuditEvents(t *testing.T, rec *httptest.ResponseRecorder) ([]nosql.AuditEvent, string) {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("audit status = %d: %s", rec.Code, rec.Body.String())
	}
	var body issue110AuditResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode audit response: %v: %s", err, rec.Body.String())
	}
	return body.Events, rec.Body.String()
}

// A readonly account must never see more than the last four characters of a
// client key in audit details; short keys are fully hidden. Admins keep the
// same fields and see the same suffix-only form for records written after the
// policy change.
func TestReadonlyAuditShowsAtMostClientKeySuffix(t *testing.T) {
	e := newIssue94Env(t)
	const longKey = "sk-live-client-secret-1234567890abcd"
	const shortKey = "short-key-1"

	if rec := e.do(t, RoleAdmin, "operator", http.MethodPost, "/data/clients", `{"name":"long-client","access_key":"`+longKey+`"}`); rec.Code != http.StatusCreated {
		t.Fatalf("create long client: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.do(t, RoleAdmin, "operator", http.MethodPut, "/data/clients/long-client", `{"name":"long-client","access_key":"`+shortKey+`"}`); rec.Code != http.StatusOK {
		t.Fatalf("update short client: %d %s", rec.Code, rec.Body.String())
	}

	for _, role := range []string{RoleReadonly, RoleAdmin} {
		events, raw := issue110AuditEvents(t, e.do(t, role, "viewer-"+role, http.MethodGet, "/data/audit", ""))
		if len(events) != 2 {
			t.Fatalf("%s audit events = %d, want 2", role, len(events))
		}
		details := map[string]string{}
		for _, event := range events {
			details[event.Action] = event.Detail
		}
		if got := details["client_create"]; got != "****abcd" {
			t.Errorf("%s client_create detail = %q, want suffix-only mask", role, got)
		}
		if got := details["client_update"]; got != "****" {
			t.Errorf("%s client_update detail = %q, want fully hidden short key", role, got)
		}
		for _, leak := range []string{longKey, shortKey, "sk-live", "shor"} {
			if strings.Contains(raw, leak) {
				t.Errorf("%s audit response leaks %q: %s", role, leak, raw)
			}
		}
		for _, event := range events {
			if event.Timestamp.IsZero() || event.Actor == "" || event.Object == "" || event.Result == "" {
				t.Errorf("%s audit event lost existing fields: %+v", role, event)
			}
		}
	}

	// The stored audit record itself must not contain the old prefix+suffix mask.
	stored, err := e.h.auditStore.List(10)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range stored {
		if event.Action == "client_create" && event.Detail != "****abcd" {
			t.Errorf("stored client_create detail = %q", event.Detail)
		}
		if event.Action == "client_update" && event.Detail != "****" {
			t.Errorf("stored client_update detail = %q", event.Detail)
		}
	}
}

// Audit rows written before the suffix-only policy still hold a prefix+suffix
// mask. The legacy mask does not encode the original key length, so a short key
// cannot be distinguished from a long one; non-admin readers therefore see only
// the fully hidden form. Admins keep the stored value for compatibility.
func TestReadonlyAuditNormalizesLegacyClientKeyMasks(t *testing.T) {
	e := newIssue94Env(t)
	legacy := []nosql.AuditEvent{
		{Action: "client_create", Object: "legacy-long", Result: "success", Detail: "abcd...wxyz"},
		{Action: "client_update", Object: "legacy-trace-style", Result: "success", Detail: "abcdef***wxyz"},
		{Action: "client_update", Object: "legacy-short", Result: "success", Detail: "***"},
		{Action: "client_delete", Object: "legacy-delete", Result: "success"},
	}
	for _, event := range legacy {
		event.Timestamp = time.Now().UTC()
		if err := e.h.auditStore.Record(event); err != nil {
			t.Fatal(err)
		}
	}

	byObject := func(role string) map[string]string {
		t.Helper()
		events, _ := issue110AuditEvents(t, e.do(t, role, "viewer-"+role, http.MethodGet, "/data/audit", ""))
		out := map[string]string{}
		for _, event := range events {
			out[event.Object] = event.Detail
		}
		return out
	}

	readonly := byObject(RoleReadonly)
	if got := readonly["legacy-long"]; got != "****" {
		t.Errorf("readonly legacy-long detail = %q, want fully hidden", got)
	}
	if got := readonly["legacy-trace-style"]; got != "****" {
		t.Errorf("readonly legacy-trace-style detail = %q, want fully hidden", got)
	}
	if got := readonly["legacy-short"]; got != "****" {
		t.Errorf("readonly legacy-short detail = %q, want fully hidden", got)
	}
	if got := readonly["legacy-delete"]; got != "" {
		t.Errorf("readonly non-key detail = %q, want empty", got)
	}

	admin := byObject(RoleAdmin)
	if got := admin["legacy-long"]; got != "abcd...wxyz" {
		t.Errorf("admin legacy-long detail = %q, want stored value unchanged", got)
	}
	if got := admin["legacy-trace-style"]; got != "abcdef***wxyz" {
		t.Errorf("admin legacy-trace-style detail = %q, want stored value unchanged", got)
	}
}

// The overview response carries the latest audit rows. It must apply the same
// role-aware redaction as the audit list; the report cache keys entries by role,
// so a readonly response cannot leak into an admin one (or vice versa).
func TestReadonlyOverviewRedactsLegacyClientKeyMasks(t *testing.T) {
	h := issue110TraceHandler(t)
	legacy := []nosql.AuditEvent{
		{Action: "client_create", Object: "legacy-long", Result: "success", Detail: "abcd...wxyz"},
		{Action: "client_update", Object: "legacy-trace-style", Result: "success", Detail: "abcdef***wxyz"},
		{Action: "client_delete", Object: "legacy-delete", Result: "success"},
	}
	for _, event := range legacy {
		event.Timestamp = time.Now().UTC()
		if err := h.auditStore.Record(event); err != nil {
			t.Fatal(err)
		}
	}

	read := func(role string) (map[string]string, string) {
		t.Helper()
		rec := issue110TraceRequest(t, h, "/api/overview", role)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s overview: status = %d: %s", role, rec.Code, rec.Body.String())
		}
		var body struct {
			RecentAudit []nosql.AuditEvent `json:"recent_audit"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s overview decode: %v: %s", role, err, rec.Body.String())
		}
		if len(body.RecentAudit) != len(legacy) {
			t.Fatalf("%s recent_audit = %d, want %d", role, len(body.RecentAudit), len(legacy))
		}
		details := map[string]string{}
		for _, event := range body.RecentAudit {
			details[event.Object] = event.Detail
		}
		return details, rec.Body.String()
	}

	readonly, readonlyRaw := read(RoleReadonly)
	if got := readonly["legacy-long"]; got != "****" {
		t.Errorf("readonly overview legacy-long detail = %q, want fully hidden", got)
	}
	if got := readonly["legacy-trace-style"]; got != "****" {
		t.Errorf("readonly overview legacy-trace-style detail = %q, want fully hidden", got)
	}
	if got := readonly["legacy-delete"]; got != "" {
		t.Errorf("readonly overview non-key detail = %q, want empty", got)
	}
	for _, leak := range []string{"abcd...wxyz", "abcdef***wxyz"} {
		if strings.Contains(readonlyRaw, leak) {
			t.Errorf("readonly overview leaks %q: %s", leak, readonlyRaw)
		}
	}

	admin, _ := read(RoleAdmin)
	if got := admin["legacy-long"]; got != "abcd...wxyz" {
		t.Errorf("admin overview legacy-long detail = %q, want stored value unchanged", got)
	}
	if got := admin["legacy-trace-style"]; got != "abcdef***wxyz" {
		t.Errorf("admin overview legacy-trace-style detail = %q, want stored value unchanged", got)
	}
}

func issue110TraceHandler(t *testing.T, records ...tracestore.TraceRecord) *Handler {
	t.Helper()
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.json")
	cfg := testConfig(tempDir, 1, []string{"k1"})
	cfg.TraceStore.Enabled = true
	cfg.TraceStore.DiskPath = filepath.Join(tempDir, "trace.log")
	if len(records) > 0 {
		var lines strings.Builder
		for _, record := range records {
			payload, err := json.Marshal(record)
			if err != nil {
				t.Fatal(err)
			}
			lines.Write(payload)
			lines.WriteByte('\n')
		}
		if err := os.WriteFile(cfg.TraceStore.DiskPath, []byte(lines.String()), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeConfigFile(t, configPath, cfg)
	stores := setupTestStores(t, tempDir)
	clients := testClients([]string{"k1"})
	seedClients(t, stores.clientStore, clients)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	authStore := auth.NewStore()
	if err := authStore.LoadFromConfig(clients); err != nil {
		t.Fatal(err)
	}
	service, err := proxy.NewService(cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })
	return NewHandler(config.NewManager(configPath), authStore, service, stores.auditStore, stores.userStore, stores.clientStore, stores.targetStore, stores.modelCostStore, stores.usageStore, nil, nil, nil, nil, logger)
}

func issue110TraceRequest(t *testing.T, h *Handler, path, role string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "http://example.com"+path, nil)
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	if role != "" {
		req = req.WithContext(ContextWithSession(req.Context(), &Session{ID: "s", Username: "viewer", Role: role}))
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// The trace list defaults to the summary projection for non-admin roles; the
// admin default stays the full record. Explicit view=summary keeps working for
// everyone and explicit view=full still normalizes client keys for non-admins.
func TestTraceListDefaultViewDependsOnRole(t *testing.T) {
	h := issue110TraceHandler(t, tracestore.TraceRecord{
		TraceID:         "legacy-trace",
		Timestamp:       time.Now().UTC(),
		ClientAccessKey: "abcdef***wxyz",
		RequestBody:     "secret-body",
	})

	list := func(path, role string) map[string]json.RawMessage {
		t.Helper()
		rec := issue110TraceRequest(t, h, path, role)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s %s: status = %d: %s", role, path, rec.Code, rec.Body.String())
		}
		var body map[string]json.RawMessage
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return body
	}

	readonly := list("/data/trace", RoleReadonly)
	if string(readonly["view"]) != `"summary"` {
		t.Errorf("readonly default view = %s, want summary", readonly["view"])
	}
	if strings.Contains(string(readonly["records"]), `"client_access_key"`) || strings.Contains(string(readonly["records"]), `"request_body":`) || strings.Contains(string(readonly["records"]), `"request_headers"`) {
		t.Errorf("readonly summary leaks full-record fields: %s", readonly["records"])
	}
	if strings.Contains(string(readonly["records"]), "secret-body") {
		t.Errorf("readonly summary leaks body: %s", readonly["records"])
	}

	admin := list("/data/trace", RoleAdmin)
	if _, ok := admin["view"]; ok {
		t.Errorf("admin default must stay full, got view=%s", admin["view"])
	}
	if !strings.Contains(string(admin["records"]), "abcdef***wxyz") {
		t.Errorf("admin default lost the stored client key field: %s", admin["records"])
	}

	sessionless := list("/data/trace", "")
	if string(sessionless["view"]) != `"summary"` {
		t.Errorf("sessionless default view = %s, want summary", sessionless["view"])
	}

	readonlyFull := list("/data/trace?view=full", RoleReadonly)
	if _, ok := readonlyFull["view"]; ok {
		t.Errorf("explicit view=full must not echo summary: %s", readonlyFull["view"])
	}
	if strings.Contains(string(readonlyFull["records"]), "abcdef***wxyz") || strings.Contains(string(readonlyFull["records"]), "wxyz") {
		t.Errorf("readonly full view leaks legacy mask material: %s", readonlyFull["records"])
	}
	if !strings.Contains(string(readonlyFull["records"]), `"client_access_key":"****"`) {
		t.Errorf("readonly full view missing fully hidden client key: %s", readonlyFull["records"])
	}

	explicitSummary := list("/data/trace?view=summary", RoleAdmin)
	if string(explicitSummary["view"]) != `"summary"` {
		t.Errorf("admin explicit summary = %s", explicitSummary["view"])
	}
}

// Detail lookups and explicit full lists must also normalize legacy client-key
// masks for non-admin roles; admins keep the stored value.
func TestTraceDetailRedactsLegacyClientKeyForNonAdmin(t *testing.T) {
	h := issue110TraceHandler(t,
		tracestore.TraceRecord{TraceID: "legacy-long", Timestamp: time.Now().UTC(), ClientAccessKey: "abcdef***wxyz"},
		tracestore.TraceRecord{TraceID: "legacy-short", Timestamp: time.Now().Add(-time.Second).UTC(), ClientAccessKey: "***"},
	)

	get := func(role, id string) tracestore.TraceRecord {
		t.Helper()
		rec := issue110TraceRequest(t, h, "/data/trace/"+id, role)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s %s: status = %d: %s", role, id, rec.Code, rec.Body.String())
		}
		var record tracestore.TraceRecord
		if err := json.Unmarshal(rec.Body.Bytes(), &record); err != nil {
			t.Fatal(err)
		}
		return record
	}

	if got := get(RoleReadonly, "legacy-long").ClientAccessKey; got != "****" {
		t.Errorf("readonly legacy-long client_access_key = %q, want fully hidden", got)
	}
	if got := get(RoleReadonly, "legacy-short").ClientAccessKey; got != "****" {
		t.Errorf("readonly legacy-short client_access_key = %q, want fully hidden", got)
	}
	if got := get(RoleAdmin, "legacy-long").ClientAccessKey; got != "abcdef***wxyz" {
		t.Errorf("admin legacy-long client_access_key = %q, want stored value", got)
	}
}

// Trace store records are shared with the proxy's ring buffer. The viewer
// projection must be a copy, or a readonly request could rewrite the stored
// record and hide the original value from later admin reads.
func TestRedactTraceRecordForViewerCopiesRecord(t *testing.T) {
	original := &tracestore.TraceRecord{TraceID: "ring", ClientAccessKey: "abcdef***wxyz"}
	redacted := redactTraceRecordForViewer(original)
	if redacted == original {
		t.Fatal("redactTraceRecordForViewer returned the original record")
	}
	if original.ClientAccessKey != "abcdef***wxyz" {
		t.Errorf("original client_access_key = %q, want stored value untouched", original.ClientAccessKey)
	}
	if redacted.ClientAccessKey != "****" {
		t.Errorf("redacted client_access_key = %q, want fully hidden", redacted.ClientAccessKey)
	}
	if redacted.TraceID != original.TraceID {
		t.Errorf("redacted trace_id = %q, want %q", redacted.TraceID, original.TraceID)
	}
	if redactTraceRecordForViewer(nil) != nil {
		t.Error("nil record must stay nil")
	}
	if redactTraceRecordsForViewer(nil) != nil {
		t.Error("nil records must stay nil")
	}
	empty := redactTraceRecordsForViewer([]*tracestore.TraceRecord{})
	if empty == nil || len(empty) != 0 {
		t.Errorf("empty records = %#v, want non-nil empty slice", empty)
	}
}
