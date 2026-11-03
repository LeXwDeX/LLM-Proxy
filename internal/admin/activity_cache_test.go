package admin

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// Issue #151: /api/activity is polled by every open dashboard; it must be
// served from a short cache (shorter than the 5s poll) instead of rescanning
// recent usage per request.
func TestActivityRouteIsShortCached(t *testing.T) {
	entry, _, _ := newNamedTargetUpdateTestHandler(t, t.TempDir(), "local", []string{"local-test"})
	h := entry.(*Handler)
	get := func() string {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/activity", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("activity: %d %s", rec.Code, rec.Body.String())
		}
		return rec.Body.String()
	}
	first := get()
	time.Sleep(20 * time.Millisecond)
	if second := get(); second != first {
		t.Fatal("second poll within the TTL was recomputed")
	}
}

func TestCachedReportTTLOverridesDefault(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	h, _ := newCountingHandler(clock)
	var n atomic.Int64
	next := h.cachedReportTTL(activityCacheTTL, func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		writeJSON(w, http.StatusOK, map[string]int64{"n": n.Load()})
	})
	serveReport(next, "/api/activity", nil)
	clock.Advance(activityCacheTTL - time.Millisecond)
	serveReport(next, "/api/activity", nil)
	if n.Load() != 1 {
		t.Fatalf("recomputed inside ttl: %d", n.Load())
	}
	clock.Advance(2 * time.Millisecond)
	serveReport(next, "/api/activity", nil)
	if n.Load() != 2 {
		t.Fatalf("not recomputed after ttl: %d", n.Load())
	}
}
