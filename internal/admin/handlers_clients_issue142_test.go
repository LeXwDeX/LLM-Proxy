package admin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

// Issue #142: the admin API must surface the client store's reserved-name
// rejection as a 400 without persisting the client.
func TestHandlerRejectsReservedUnknownClientNameIssue142(t *testing.T) {
	h, stores, _ := newNamedTargetUpdateTestHandler(t, t.TempDir(), "primary", []string{"sk-primary-real-0001"})

	postClient := func(client config.Client) *httptest.ResponseRecorder {
		t.Helper()
		payload, err := json.Marshal(client)
		if err != nil {
			t.Fatalf("marshal client: %v", err)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "http://example.com/data/clients", bytes.NewReader(payload)))
		return rec
	}
	putClient := func(name, clientName, accessKey string) *httptest.ResponseRecorder {
		t.Helper()
		payload, err := json.Marshal(config.Client{Name: clientName, AccessKey: accessKey})
		if err != nil {
			t.Fatalf("marshal client: %v", err)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "http://example.com/data/clients/"+name, bytes.NewReader(payload)))
		return rec
	}

	for i, name := range []string{"unknown", "Unknown", "UNKNOWN"} {
		rec := postClient(config.Client{Name: name, AccessKey: fmt.Sprintf("reserved-%d", i)})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("POST client %q expected 400, got %d body=%s", name, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "is reserved") {
			t.Fatalf("POST client %q body = %s, want reserved-name error", name, rec.Body.String())
		}
	}

	rec := postClient(config.Client{Name: "unknown-2", AccessKey: "unknown-2-key"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST legal client expected 201, got %d body=%s", rec.Code, rec.Body.String())
	}

	rec = putClient("unknown", "unknown", "unknown-key")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PUT client unknown expected 400, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "is reserved") {
		t.Fatalf("PUT client unknown body = %s, want reserved-name error", rec.Body.String())
	}

	// Renaming an existing client to a reserved name is rejected too.
	rec = putClient("unknown-2", "UNKNOWN", "unknown-2-key")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PUT client renamed to UNKNOWN expected 400, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "is reserved") {
		t.Fatalf("PUT rename body = %s, want reserved-name error", rec.Body.String())
	}

	// Legal names, including those containing the substring, keep working.
	rec = putClient("unknown-2", "unknown-2", "unknown-2-key")
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT legal client expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	stored, err := stores.clientStore.Get("unknown-2")
	if err != nil {
		t.Fatalf("get legal client: %v", err)
	}
	if stored == nil || stored.Name != "unknown-2" {
		t.Fatalf("legal client not persisted correctly: %+v", stored)
	}
	if reserved, err := stores.clientStore.Get("unknown"); err != nil || reserved != nil {
		t.Fatalf("reserved client must not be stored: %+v, %v", reserved, err)
	}
}
