package admin

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/proxy"
)

const issue68Header = "X-Target-Key-Snapshot"

type issue68Target struct {
	Snapshot string `json:"key_snapshot"`
	Indices  []*int `json:"key_runtime_indices"`
}

func issue68Get(t *testing.T, h http.Handler) issue68Target {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://example.com/data/targets", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list targets: status %d", rec.Code)
	}
	var body struct {
		Targets []issue68Target `json:"targets"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || len(body.Targets) != 1 {
		t.Fatalf("decode target list: %v", err)
	}
	return body.Targets[0]
}

func issue68Request(t *testing.T, h http.Handler, method, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var data []byte
	if body != nil {
		var err error
		data, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, "http://example.com/data/targets/target-1"+path, bytes.NewReader(data))
	if token != "" {
		req.Header.Set(issue68Header, token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func issue68Snapshot(t *testing.T, h http.Handler) string {
	t.Helper()
	snapshot := issue68Get(t, h).Snapshot
	if snapshot == "" {
		t.Error("missing opaque key snapshot")
		return "missing-baseline-snapshot"
	}
	return snapshot
}

func issue68Int(i int) *int { return &i }

func issue68Handler(t *testing.T, keys []string) (http.Handler, testStores, *proxy.Service) {
	t.Helper()
	h, _, stores, service := newTargetUpdateTestHandlerWithService(t, t.TempDir(), keys)
	cfg, err := h.(*Handler).currentConfig()
	if err != nil {
		t.Fatal(err)
	}
	if err := service.ApplyConfig(cfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })
	return h, stores, service
}

func TestIssue68SnapshotMappingAndSecret(t *testing.T) {
	keys := []string{"abcd-first-zzzz", "abcd-other-zzzz", "", "abcd-first-zzzz", "last-key-1234"}
	h, _, _ := issue68Handler(t, keys)
	got := issue68Get(t, h)
	if got.Snapshot == "" {
		t.Error("missing opaque key snapshot")
	}
	want := []*int{issue68Int(0), issue68Int(1), nil, issue68Int(0), issue68Int(2)}
	if !reflect.DeepEqual(got.Indices, want) {
		t.Errorf("runtime mapping = %v, want %v", got.Indices, want)
	}
	for _, key := range keys {
		if key != "" && strings.Contains(got.Snapshot, key) {
			t.Error("snapshot contains a key")
		}
	}
	h2, _, _ := issue68Handler(t, keys)
	if other := issue68Get(t, h2).Snapshot; other == got.Snapshot {
		t.Error("independent handlers shared a snapshot")
	}
}

func TestIssue68StaleSnapshotsDoNotModifyKeys(t *testing.T) {
	for _, change := range []struct {
		name string
		keys []string
	}{
		{"reorder", []string{"sk-third-0003", "sk-second-0002"}},
		{"replace", []string{"sk-replacement-0004", "sk-third-0003"}},
	} {
		t.Run(change.name, func(t *testing.T) {
			keys := []string{"sk-primary-0001", "sk-second-0002", "sk-third-0003"}
			h, stores, service := issue68Handler(t, keys)
			token := issue68Snapshot(t, h)
			rec := issue68Request(t, h, http.MethodPut, "", "", targetUpdateBody(keys[0], change.keys))
			if rec.Code != http.StatusOK {
				t.Fatalf("setup update: %d", rec.Code)
			}
			before := mustGetTarget(t, stores.targetStore, "target-1")
			rec = issue68Request(t, h, http.MethodPut, "", token, targetUpdateBody("__existing_key_index__:0", []string{"__existing_key_index__:1"}))
			if rec.Code != http.StatusConflict {
				t.Errorf("stale update status = %d, want 409", rec.Code)
			}
			after := mustGetTarget(t, stores.targetStore, "target-1")
			if !reflect.DeepEqual(before.APIKeys, after.APIKeys) || before.APIKey != after.APIKey {
				t.Error("stale update changed stored keys")
			}
			rec = issue68Request(t, h, http.MethodPut, "/keys/1/block", token, nil)
			if rec.Code != http.StatusConflict {
				t.Errorf("stale block status = %d, want 409", rec.Code)
			}
			for _, status := range service.KeyPoolStatus("target-1") {
				if status.Blocked {
					t.Error("stale block changed runtime")
				}
			}
		})
	}
}

func TestIssue68EqualMasksAndDuplicateSlots(t *testing.T) {
	keys := []string{"abcd-first-zzzz", "abcd-other-zzzz", "", "abcd-first-zzzz"}
	h, stores, _ := issue68Handler(t, keys)
	token := issue68Snapshot(t, h)
	rec := issue68Request(t, h, http.MethodPut, "", token, targetUpdateBody("__existing_key_index__:0", []string{"__existing_key_index__:1", "__existing_key_index__:2", "__existing_key_index__:3"}))
	if rec.Code != http.StatusOK {
		t.Fatalf("conditional update status = %d body=%s", rec.Code, rec.Body.String())
	}
	got := mustGetTarget(t, stores.targetStore, "target-1")
	if got.APIKey != keys[0] || !reflect.DeepEqual(got.APIKeys, []string{keys[1], "", keys[0]}) {
		t.Error("indexed references did not preserve exact slots")
	}
}

func TestIssue68SameNameRecreationAndABA(t *testing.T) {
	for _, operation := range []string{"aba", "recreate"} {
		t.Run(operation, func(t *testing.T) {
			keys := []string{"sk-primary-0001", "sk-second-0002"}
			h, stores, _ := issue68Handler(t, keys)
			token := issue68Snapshot(t, h)
			if operation == "aba" {
				if rec := issue68Request(t, h, http.MethodPut, "", "", targetUpdateBody(keys[0], []string{"replacement-0003"})); rec.Code != http.StatusOK {
					t.Fatal(rec.Code)
				}
				if rec := issue68Request(t, h, http.MethodPut, "", "", targetUpdateBody(keys[0], keys[1:])); rec.Code != http.StatusOK {
					t.Fatal(rec.Code)
				}
			} else {
				original := mustGetTarget(t, stores.targetStore, "target-1")
				if rec := issue68Request(t, h, http.MethodDelete, "", "", nil); rec.Code != http.StatusOK {
					t.Fatal(rec.Code)
				}
				payload, _ := json.Marshal(original)
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "http://example.com/data/targets", bytes.NewReader(payload)))
				if rec.Code != http.StatusCreated {
					t.Fatalf("recreate: %d %s", rec.Code, rec.Body.String())
				}
			}
			rec := issue68Request(t, h, http.MethodPut, "/keys/0/block", token, nil)
			if rec.Code != http.StatusConflict {
				t.Errorf("old snapshot after %s status = %d", operation, rec.Code)
			}
		})
	}
}

func TestIssue68RuntimeStoreMismatch(t *testing.T) {
	keys := []string{"sk-primary-0001", "sk-second-0002"}
	h, stores, service := issue68Handler(t, keys)
	token := issue68Snapshot(t, h)
	stored := mustGetTarget(t, stores.targetStore, "target-1")
	other := stored
	other.APIKeys = []string{"different-key-0003"}
	if err := service.UpdateTargets([]config.Target{other}); err != nil {
		t.Fatal(err)
	}
	got := issue68Get(t, h)
	if got.Snapshot != "" || len(got.Indices) != 0 {
		t.Error("issued snapshot despite runtime/store mismatch")
	}
	for _, path := range []string{"", "/keys/0/block", "/keys/0/unblock"} {
		var body any
		if path == "" {
			body = targetUpdateBody("__existing_key_index__:0", []string{"__existing_key_index__:1"})
		}
		rec := issue68Request(t, h, http.MethodPut, path, token, body)
		if rec.Code != http.StatusConflict {
			t.Errorf("mismatch %s status = %d", path, rec.Code)
		}
	}
	if got := mustGetTarget(t, stores.targetStore, "target-1"); !reflect.DeepEqual(got.APIKeys, stored.APIKeys) {
		t.Error("mismatch changed store")
	}
}

func TestIssue68RuntimeEndpointMismatchWithSameKeys(t *testing.T) {
	h, stores, service := issue68Handler(t, []string{"primary-0001", "secondary-0002"})
	token := issue68Snapshot(t, h)
	target := mustGetTarget(t, stores.targetStore, "target-1")
	target.Endpoint = "https://different.example.com"
	if err := service.UpdateTargets([]config.Target{target}); err != nil {
		t.Fatal(err)
	}
	if got := issue68Get(t, h); got.Snapshot != "" || len(got.Indices) != 0 {
		t.Error("issued snapshot for a different runtime endpoint")
	}
	if rec := issue68Request(t, h, http.MethodPut, "/keys/0/block", token, nil); rec.Code != http.StatusConflict {
		t.Errorf("mismatched runtime endpoint: status %d", rec.Code)
	}
}

func TestIssue68ConditionalBlockAndUnblock(t *testing.T) {
	h, _, service := issue68Handler(t, []string{"primary-0001", "secondary-0002"})
	token := issue68Snapshot(t, h)
	rec := issue68Request(t, h, http.MethodPut, "/keys/1/block", token, nil)
	if rec.Code != http.StatusOK || !service.KeyPoolStatus("target-1")[1].Blocked {
		t.Fatalf("block failed: %d %s", rec.Code, rec.Body.String())
	}
	if rec := issue68Request(t, h, http.MethodPut, "/keys/1/unblock", token, nil); rec.Code != http.StatusConflict || !service.KeyPoolStatus("target-1")[1].Blocked {
		t.Error("stale snapshot undid the newer block")
	}
	token = issue68Snapshot(t, h)
	rec = issue68Request(t, h, http.MethodPut, "/keys/1/unblock", token, nil)
	if rec.Code != http.StatusOK || service.KeyPoolStatus("target-1")[1].Blocked {
		t.Fatalf("unblock failed: %d %s", rec.Code, rec.Body.String())
	}
	token = issue68Snapshot(t, h)
	for _, values := range [][]string{{""}, {token, token}, {token + "x"}} {
		req := httptest.NewRequest(http.MethodPut, "http://example.com/data/targets/target-1/keys/0/block", nil)
		for _, value := range values {
			req.Header.Add(issue68Header, value)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusConflict {
			t.Errorf("invalid precondition returned %d", rec.Code)
		}
	}
	if service.KeyPoolStatus("target-1")[0].Blocked {
		t.Error("invalid precondition blocked a key")
	}
}

func issue68Successor(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("key action returned %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		OK       bool   `json:"ok"`
		Snapshot string `json:"key_snapshot"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || !body.OK || body.Snapshot == "" {
		t.Fatalf("successful conditional key action returned no successor snapshot: %s", rec.Body.String())
	}
	return body.Snapshot
}

func TestIssue68ConditionalSuccessorSnapshot(t *testing.T) {
	keys := []string{"primary-0001", "secondary-0002", "", "primary-0001"}
	h, stores, service := issue68Handler(t, keys)
	before := issue68Get(t, h)
	if before.Snapshot == "" {
		t.Fatal("missing original snapshot")
	}

	block := issue68Request(t, h, http.MethodPut, "/keys/1/block", before.Snapshot, nil)
	afterBlock := issue68Successor(t, block)
	if !service.KeyPoolStatus("target-1")[1].Blocked {
		t.Fatal("key was not blocked")
	}
	current := issue68Get(t, h)
	if afterBlock == before.Snapshot || afterBlock != current.Snapshot || !reflect.DeepEqual(before.Indices, current.Indices) {
		t.Fatal("block successor does not match the same slots and runtime mapping")
	}
	for _, key := range keys {
		if key != "" && strings.Contains(afterBlock, key) {
			t.Fatal("successor contains a key")
		}
	}
	if rec := issue68Request(t, h, http.MethodPut, "/keys/1/unblock", before.Snapshot, nil); rec.Code != http.StatusConflict {
		t.Fatalf("old token after block returned %d", rec.Code)
	}

	unblock := issue68Request(t, h, http.MethodPut, "/keys/1/unblock", afterBlock, nil)
	afterUnblock := issue68Successor(t, unblock)
	if service.KeyPoolStatus("target-1")[1].Blocked {
		t.Fatal("key was not unblocked")
	}
	current = issue68Get(t, h)
	if afterUnblock == afterBlock || afterUnblock != current.Snapshot || !reflect.DeepEqual(before.Indices, current.Indices) {
		t.Fatal("unblock successor does not match the same slots and runtime mapping")
	}
	if rec := issue68Request(t, h, http.MethodPut, "/keys/1/block", afterBlock, nil); rec.Code != http.StatusConflict {
		t.Fatalf("old token after unblock returned %d", rec.Code)
	}

	body := targetUpdateBody("__existing_key_index__:0", []string{"__existing_key_index__:1", "__existing_key_index__:2", "__existing_key_index__:3"})
	if rec := issue68Request(t, h, http.MethodPut, "", afterUnblock, body); rec.Code != http.StatusOK {
		t.Fatalf("successor did not allow the original draft: %d %s", rec.Code, rec.Body.String())
	}
	stored := mustGetTarget(t, stores.targetStore, "target-1")
	if stored.APIKey != keys[0] || !reflect.DeepEqual(stored.APIKeys, keys[1:]) {
		t.Fatal("successor changed the original slots")
	}
	for _, path := range []string{"", "/keys/1/block", "/keys/1/unblock"} {
		rec := issue68Request(t, h, http.MethodPut, path, afterUnblock, body)
		if rec.Code != http.StatusConflict {
			t.Errorf("successor reused after configuration update for %s: %d", path, rec.Code)
		}
	}
}

func TestIssue68UnconditionalKeyActionResponseUnchanged(t *testing.T) {
	h, _, _ := issue68Handler(t, []string{"primary-0001", "secondary-0002"})
	for _, action := range []string{"block", "unblock"} {
		rec := issue68Request(t, h, http.MethodPut, "/keys/1/"+action, "", nil)
		var body map[string]any
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &body) != nil || !reflect.DeepEqual(body, map[string]any{"ok": true}) {
			t.Errorf("legacy %s response: %d %s", action, rec.Code, rec.Body.String())
		}
	}
}

func TestIssue68SuccessorRejectsInterveningRuntimeChanges(t *testing.T) {
	for _, action := range []string{"block", "unblock"} {
		for _, change := range []string{"extra-key-action", "runtime-aba"} {
			t.Run(action+"/"+change, func(t *testing.T) {
				raw, stores, service := issue68Handler(t, []string{"primary-0001", "secondary-0002"})
				h := raw.(*Handler)
				if action == "unblock" {
					if err := service.BlockKey("target-1", 1); err != nil {
						t.Fatal(err)
					}
				}
				original := mustGetTarget(t, stores.targetStore, "target-1")
				token := issue68Snapshot(t, h)

				// A separate audit database lets the action finish while its audit write waits.
				audit := setupTestStores(t, t.TempDir())
				h.auditStore = audit.auditStore
				tx, err := audit.db.Begin(true)
				if err != nil {
					t.Fatal(err)
				}
				done := make(chan *httptest.ResponseRecorder, 1)
				finished := false
				defer func() {
					_ = tx.Rollback()
					if !finished {
						select {
						case <-done:
						case <-time.After(5 * time.Second):
							t.Error("key action did not finish after audit was released")
						}
					}
				}()
				go func() {
					req := httptest.NewRequest(http.MethodPut, "http://example.com/data/targets/target-1/keys/1/"+action, nil)
					req.Header.Set(issue68Header, token)
					rec := httptest.NewRecorder()
					h.ServeHTTP(rec, req)
					done <- rec
				}()

				deadline := time.Now().Add(5 * time.Second)
				for service.KeyPoolStatus("target-1")[1].Blocked != (action == "block") {
					if time.Now().After(deadline) {
						t.Fatal("key action did not reach the blocked audit write")
					}
					time.Sleep(time.Millisecond)
				}
				select {
				case <-done:
					finished = true
					t.Fatal("key action completed while audit was blocked")
				default:
				}

				if change == "extra-key-action" {
					if err := service.BlockKey("target-1", 0); err != nil {
						t.Fatal(err)
					}
				} else {
					changed := original
					changed.APIKeys = []string{"replacement-0003"}
					if err := service.UpdateTargets([]config.Target{changed}); err != nil {
						t.Fatal(err)
					}
					if err := service.UpdateTargets([]config.Target{original}); err != nil {
						t.Fatal(err)
					}
				}
				if err := tx.Rollback(); err != nil {
					t.Fatal(err)
				}
				var rec *httptest.ResponseRecorder
				select {
				case rec = <-done:
					finished = true
				case <-time.After(5 * time.Second):
					t.Fatal("key action did not finish")
				}
				var body map[string]json.RawMessage
				if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &body) != nil || string(body["key_snapshot"]) != "null" {
					t.Errorf("intervening change must yield a null successor: %d %s", rec.Code, rec.Body.String())
				}
			})
		}
	}
}

func TestIssue68ReadonlySnapshotCannotWrite(t *testing.T) {
	raw, _, service := issue68Handler(t, []string{"primary-0001", "secondary-0002"})
	h := raw.(*Handler)
	sm := NewSessionManager(config.AdminSessionConfig{Secret: "test-session-secret", CookieName: "session", TTLSeconds: 3600}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.sessionManager = sm
	h.router = h.routes()
	secured := sm.Middleware(h)
	_, cookie, err := sm.CreateSession("viewer", RoleReadonly)
	if err != nil {
		t.Fatal(err)
	}
	get := httptest.NewRequest(http.MethodGet, "http://example.com/data/targets", nil)
	get.AddCookie(cookie)
	rec := httptest.NewRecorder()
	secured.ServeHTTP(rec, get)
	var list struct {
		Targets []issue68Target `json:"targets"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &list) != nil || len(list.Targets) != 1 || list.Targets[0].Snapshot == "" {
		t.Fatal("readonly could not read target snapshot")
	}
	for _, path := range []string{"", "/keys/0/block", "/keys/0/unblock"} {
		req := httptest.NewRequest(http.MethodPut, "http://example.com/data/targets/target-1"+path, strings.NewReader(`{}`))
		req.Header.Set(issue68Header, list.Targets[0].Snapshot)
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		secured.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("readonly %s status %d", path, rec.Code)
		}
	}
	if service.KeyPoolStatus("target-1")[0].Blocked {
		t.Error("readonly request changed runtime")
	}
}

type issue68BlockedBody struct {
	ready   chan struct{}
	release chan struct{}
	data    *bytes.Reader
}

func (b *issue68BlockedBody) Read(p []byte) (int, error) {
	select {
	case <-b.ready:
	default:
		close(b.ready)
		<-b.release
	}
	return b.data.Read(p)
}
func (b *issue68BlockedBody) Close() error { return nil }

type issue68BlockedWriter struct {
	*httptest.ResponseRecorder
	ready   chan struct{}
	release chan struct{}
}

func (w *issue68BlockedWriter) Write(p []byte) (int, error) {
	close(w.ready)
	<-w.release
	return w.ResponseRecorder.Write(p)
}

func TestIssue68SlowListDoesNotBlockTargetUpdates(t *testing.T) {
	h, _, _ := issue68Handler(t, []string{"primary-0001", "secondary-0002"})
	w := &issue68BlockedWriter{ResponseRecorder: httptest.NewRecorder(), ready: make(chan struct{}), release: make(chan struct{})}
	getDone := make(chan struct{})
	go func() {
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "http://example.com/data/targets", nil))
		close(getDone)
	}()
	<-w.ready
	payload, _ := json.Marshal(targetUpdateBody("primary-0001", []string{"replacement-0003"}))
	putDone := make(chan int, 1)
	go func() {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "http://example.com/data/targets/target-1", bytes.NewReader(payload)))
		putDone <- rec.Code
	}()
	select {
	case status := <-putDone:
		if status != http.StatusOK {
			t.Errorf("update status %d", status)
		}
	case <-time.After(2 * time.Second):
		t.Error("slow list response held the configuration lock")
		close(w.release)
		<-putDone
		<-getDone
		return
	}
	close(w.release)
	<-getDone
	var list struct {
		Targets []issue68Target `json:"targets"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || len(list.Targets) != 1 {
		t.Fatal("bad list response")
	}
	if rec := issue68Request(t, h, http.MethodPut, "/keys/1/block", list.Targets[0].Snapshot, nil); rec.Code != http.StatusConflict {
		t.Errorf("snapshot from before the update returned %d", rec.Code)
	}
}

func TestIssue68GetAndKeyActionsWaitForPut(t *testing.T) {
	for _, action := range []string{"get", "block", "unblock"} {
		t.Run(action, func(t *testing.T) {
			keys := []string{"sk-primary-0001", "sk-second-0002"}
			h, _, service := issue68Handler(t, keys)
			if action == "unblock" {
				if err := service.BlockKey("target-1", 1); err != nil {
					t.Fatal(err)
				}
			}
			old := issue68Snapshot(t, h)
			payload, _ := json.Marshal(targetUpdateBody(keys[0], []string{"sk-replacement-0003"}))
			body := &issue68BlockedBody{ready: make(chan struct{}), release: make(chan struct{}), data: bytes.NewReader(payload)}
			req := httptest.NewRequest(http.MethodPut, "http://example.com/data/targets/target-1", body)
			putDone := make(chan int, 1)
			go func() { rec := httptest.NewRecorder(); h.ServeHTTP(rec, req); putDone <- rec.Code }()
			<-body.ready
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				rec := httptest.NewRecorder()
				path := "http://example.com/data/targets"
				method := http.MethodGet
				if action != "get" {
					method = http.MethodPut
					path += "/target-1/keys/1/" + action
				}
				r := httptest.NewRequest(method, path, nil)
				r.Header.Set(issue68Header, old)
				h.ServeHTTP(rec, r)
				done <- rec
			}()
			select {
			case rec := <-done:
				t.Errorf("%s completed during blocked PUT: %d", action, rec.Code)
			case <-time.After(75 * time.Millisecond):
			}
			close(body.release)
			if code := <-putDone; code != http.StatusOK {
				t.Fatalf("put status=%d", code)
			}
			select {
			case rec := <-done:
				if action == "get" {
					var list struct {
						Targets []issue68Target `json:"targets"`
					}
					if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || len(list.Targets) != 1 || list.Targets[0].Snapshot == old {
						t.Error("GET did not return the new generation")
					}
				} else if rec.Code != http.StatusConflict {
					t.Errorf("%s after PUT status=%d", action, rec.Code)
				}
			case <-time.After(time.Second):
				t.Error("action did not complete")
			}
		})
	}
}

var _ io.ReadCloser = (*issue68BlockedBody)(nil)

func TestReviewSnapshotAfterEmptyFieldsPersist(t *testing.T) {
	h, stores, service := issue68Handler(t, []string{"fixture-primary", "fixture-second"})
	body := targetUpdateBody("__existing_key_index__:0", []string{"__existing_key_index__:1"})
	body["custom_headers"] = map[string]string{}
	body["custom_body"] = map[string]any{}
	body["model_mappings"] = []any{}
	if rec := issue68Request(t, h, http.MethodPut, "", issue68Snapshot(t, h), body); rec.Code != http.StatusOK {
		t.Fatalf("first update: %d", rec.Code)
	}
	token := issue68Get(t, h).Snapshot
	if token == "" {
		t.Fatal("missing snapshot after persisted empty fields")
	}
	if rec := issue68Request(t, h, http.MethodPut, "", token, body); rec.Code != http.StatusOK {
		t.Fatalf("second update: %d", rec.Code)
	}
	stored := mustGetTarget(t, stores.targetStore, "target-1")
	stored.CustomHeaders = map[string]string{"X-Changed": "yes"}
	if err := service.UpdateTargets([]config.Target{stored}); err != nil {
		t.Fatal(err)
	}
	if rec := issue68Request(t, h, http.MethodPut, "", token, body); rec.Code != http.StatusConflict {
		t.Fatalf("changed runtime accepted stale snapshot: %d", rec.Code)
	}
}

func TestReviewSnapshotCreateWithEmptyFieldsAndExtraKeys(t *testing.T) {
	h, stores, _ := issue68Handler(t, []string{"fixture-primary"})
	var logs bytes.Buffer
	h.(*Handler).logger = slog.New(slog.NewTextHandler(&logs, nil))
	if rec := issue68Request(t, h, http.MethodDelete, "", "", nil); rec.Code != http.StatusOK {
		t.Fatalf("delete setup: %d", rec.Code)
	}
	create := map[string]any{
		"name": "target-1", "endpoint_type": "openai", "usage_adapter": "standard",
		"endpoint": "https://example.com", "api_key": "", "api_keys": []string{"fixture-a", "fixture-b"},
		"custom_headers": map[string]string{}, "custom_body": map[string]any{}, "model_mappings": []any{},
	}
	create["api_keys"] = []string{"", " "}
	data, err := json.Marshal(create)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "http://example.com/data/targets", bytes.NewReader(data)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("blank create: %d", rec.Code)
	}
	create["api_keys"] = []string{"fixture-a", "fixture-b"}
	data, err = json.Marshal(create)
	if err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "http://example.com/data/targets", bytes.NewReader(data)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "fixture-a") || strings.Contains(rec.Body.String(), "fixture-b") {
		t.Fatal("response exposed key")
	}
	token := issue68Get(t, h).Snapshot
	if token == "" {
		t.Fatal("missing snapshot after create")
	}
	body := map[string]any{"api_key": "__existing_key_index__:0", "api_keys": []string{"__existing_key_index__:1", "__existing_key_index__:2"}, "max_concurrency": 2}
	if rec := issue68Request(t, h, http.MethodPut, "", token, body); rec.Code != http.StatusOK {
		t.Fatalf("conditional update: %d", rec.Code)
	}
	got := mustGetTarget(t, stores.targetStore, "target-1")
	if got.APIKey != "" || !reflect.DeepEqual(got.APIKeys, []string{"fixture-a", "fixture-b"}) || got.AllowBearer {
		t.Fatal("key slots changed")
	}
	events, err := stores.auditStore.List(50)
	if err != nil {
		t.Fatal(err)
	}
	audit, err := json.Marshal(events)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"fixture-a", "fixture-b"} {
		if strings.Contains(logs.String(), key) || bytes.Contains(audit, []byte(key)) {
			t.Fatal("key exposed in logs or audit")
		}
	}
}

func TestReviewSnapshotEmptyPrimaryWithExtraKeys(t *testing.T) {
	h, stores, _ := issue68Handler(t, []string{"", "fixture-a", "fixture-b"})
	body := targetUpdateBody("__existing_key_index__:0", []string{"__existing_key_index__:1", "__existing_key_index__:2"})
	body["max_concurrency"] = 3
	if rec := issue68Request(t, h, http.MethodPut, "", issue68Snapshot(t, h), body); rec.Code != http.StatusOK {
		t.Fatalf("update with extra keys: %d", rec.Code)
	}
	got := mustGetTarget(t, stores.targetStore, "target-1")
	if got.APIKey != "" || !reflect.DeepEqual(got.APIKeys, []string{"fixture-a", "fixture-b"}) || got.AllowBearer || got.MaxConcurrency != 3 {
		t.Fatal("update changed key slots or bearer setting")
	}
	body["api_key"] = ""
	body["api_keys"] = []string{" ", ""}
	if rec := issue68Request(t, h, http.MethodPut, "", issue68Snapshot(t, h), body); rec.Code != http.StatusBadRequest {
		t.Fatalf("blank keys status: %d", rec.Code)
	}
}

func TestReviewSingleExtraKeyForwardsAfterAdminSave(t *testing.T) {
	for _, provider := range []string{"openai", "azure_openai"} {
		for _, mode := range []string{"create", "reduce", "duplicates"} {
			t.Run(provider+"/"+mode, func(t *testing.T) {
				const key = "fixture-only-key"
				headers := make(chan bool, 3)
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					valid := r.Header.Get("Authorization") == "Bearer "+key
					if provider == "azure_openai" {
						valid = r.Header.Get("Api-Key") == key
					}
					headers <- valid
					w.Header().Set("Content-Type", "application/json")
					if !valid {
						w.WriteHeader(http.StatusUnauthorized)
						return
					}
					_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
				}))
				defer upstream.Close()
				keys := []string{"", key, "fixture-other-key"}
				if mode == "duplicates" {
					keys[2] = key
				}
				h, stores, service := issue68Handler(t, keys)
				if mode == "create" {
					if rec := issue68Request(t, h, http.MethodDelete, "", "", nil); rec.Code != http.StatusOK {
						t.Fatalf("delete: %d", rec.Code)
					}
					body := map[string]any{"name": "target-1", "endpoint_type": provider, "usage_adapter": "standard", "endpoint": upstream.URL, "api_key": "", "api_keys": []string{key}, "model_mappings": []any{}}
					data, err := json.Marshal(body)
					if err != nil {
						t.Fatal(err)
					}
					rec := httptest.NewRecorder()
					h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "http://example.com/data/targets", bytes.NewReader(data)))
					if rec.Code != http.StatusCreated {
						t.Fatalf("create: %d", rec.Code)
					}
				} else {
					refs := []string{"__existing_key_index__:1"}
					if mode == "duplicates" {
						refs = append(refs, "__existing_key_index__:2")
					}
					body := map[string]any{"endpoint_type": provider, "endpoint": upstream.URL, "api_key": "__existing_key_index__:0", "api_keys": refs, "model_mappings": []any{}}
					if rec := issue68Request(t, h, http.MethodPut, "", issue68Snapshot(t, h), body); rec.Code != http.StatusOK {
						t.Fatalf("update: %d", rec.Code)
					}
				}
				stored := mustGetTarget(t, stores.targetStore, "target-1")
				want := []string{key}
				if mode == "duplicates" {
					want = append(want, key)
				}
				if stored.APIKey != "" || !reflect.DeepEqual(stored.APIKeys, want) || stored.AllowBearer {
					t.Fatal("stored slots or bearer changed")
				}
				got := issue68Get(t, h)
				if got.Snapshot == "" || len(got.Indices) != 1+len(want) {
					t.Fatal("snapshot unavailable")
				}
				for _, index := range got.Indices {
					if index != nil {
						t.Fatal("single key must not advertise pool indices")
					}
				}
				if service.KeyPoolStatus("target-1") != nil {
					t.Fatal("single effective key created a pool")
				}
				req := httptest.NewRequest(http.MethodPost, "http://example.com/v1/chat/completions", strings.NewReader(`{"model":"qwen-max","messages":[{"role":"user","content":"hi"}]}`))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Authorization", "Bearer k1")
				rec := httptest.NewRecorder()
				handler := h.(*Handler)
				auth.Middleware(handler.authStore, handler.logger)(service).ServeHTTP(rec, req)
				if rec.Code != http.StatusOK {
					t.Errorf("normal request status: %d", rec.Code)
				}
				select {
				case valid := <-headers:
					if !valid {
						t.Error("normal request used incorrect upstream credential")
					}
				default:
					t.Error("normal request did not reach upstream")
				}
				result, err := service.TestModel("target-1", "qwen-max")
				if err != nil || result == nil || !result.Success {
					t.Error("model test failed")
				}
				select {
				case valid := <-headers:
					if !valid {
						t.Error("model test used incorrect upstream credential")
					}
				default:
					t.Error("model test did not reach upstream")
				}
			})
		}
	}
}
