package proxy

import (
	"encoding/json"
	"errors"
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
)

func TestReviewImportedSingleExtraKeyForwards(t *testing.T) {
	const key = "fixture-single-key"
	requests := make(chan bool, 2)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r.Header.Get("Authorization") == "Bearer "+key
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer upstream.Close()
	original := config.Target{Name: "target", EndpointType: "openai", Endpoint: upstream.URL, APIKeys: []string{key}}
	data, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	var target config.Target
	if err := json.Unmarshal(data, &target); err != nil {
		t.Fatal(err)
	}
	s, err := NewService(&config.Config{Targets: []config.Target{target}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	_, statuses, indices, ok := s.TargetKeySnapshot(target)
	if !ok || statuses != nil || len(indices) != 2 || indices[0] != nil || indices[1] != nil {
		t.Fatal("invalid single-key snapshot")
	}
	status := s.TargetStatuses(time.Now())
	if len(status) != 1 || status[0].TotalKeys != 1 || status[0].ActiveKeys != 1 {
		t.Fatal("single key status incorrect")
	}
	req := httptest.NewRequest(http.MethodPost, "http://example.com/v1/chat/completions", strings.NewReader(`{"model":"qwen-max","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(auth.WithPrincipal(req.Context(), newAllowAllPrincipal(t, "fixture-client")))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("normal request status: %d", rec.Code)
	}
	select {
	case valid := <-requests:
		if !valid {
			t.Error("incorrect upstream credential")
		}
	default:
		t.Error("normal request did not reach upstream")
	}
}

func TestReviewSnapshotPersistenceEquivalence(t *testing.T) {
	for _, body := range []map[string]any{{}, {"nested": map[string]any{"number": 3, "array": []any{1, nil}}}} {
		target := config.Target{Name: "target", EndpointType: "openai", Endpoint: "https://example.com", APIKey: "one", CustomHeaders: map[string]string{}, CustomBody: body, ModelMappings: []config.ModelMapping{}}
		s, err := NewService(&config.Config{Targets: []config.Target{target}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err != nil {
			t.Fatal(err)
		}
		data, err := json.Marshal(target)
		if err != nil {
			t.Fatal(err)
		}
		var persisted config.Target
		if err := json.Unmarshal(data, &persisted); err != nil {
			t.Fatal(err)
		}
		if _, _, _, ok := s.TargetKeySnapshot(persisted); !ok {
			t.Error("persistence-equivalent target rejected")
		}
		persisted.CustomBody = map[string]any{"changed": true}
		if _, _, _, ok := s.TargetKeySnapshot(persisted); ok {
			t.Error("changed body accepted")
		}
		_ = s.Close()
	}
}

func TestIssue68RuntimeSnapshotRevision(t *testing.T) {
	target := config.Target{Name: "target", EndpointType: "openai", Endpoint: "https://example.com", APIKey: "primary", APIKeys: []string{"", "second", "primary", "third"}}
	cfg := &config.Config{Targets: []config.Target{target}}
	s, err := NewService(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	revision, statuses, slots, ok := s.TargetKeySnapshot(target)
	if !ok || revision == 0 || len(statuses) != 3 || len(slots) != 5 {
		t.Fatal("invalid initial snapshot")
	}
	indices := []int{-1, -1, -1, -1, -1}
	for i, index := range slots {
		if index != nil {
			indices[i] = *index
		}
	}
	if !reflect.DeepEqual(indices, []int{0, -1, 1, 0, 2}) {
		t.Errorf("indices %v", indices)
	}
	if err := s.SetKeyBlockedIfTargetRevision("target", 1, true, revision); err != nil {
		t.Fatal(err)
	}
	if !s.KeyPoolStatus("target")[1].Blocked {
		t.Fatal("current revision failed to block key")
	}
	changed := target
	changed.APIKeys = []string{"third", "second"}
	if err := s.UpdateTargets([]config.Target{changed}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetKeyBlockedIfTargetRevision("target", 1, false, revision); !errors.Is(err, ErrTargetSnapshotChanged) {
		t.Errorf("stale unblock error = %v", err)
	}
	status := s.KeyPoolStatus("target")
	if status[1].Blocked || !status[2].Blocked {
		t.Error("stale unblock changed a runtime key")
	}
	if err := s.ApplyConfigIfTargetRevision(cfg, revision); !errors.Is(err, ErrTargetSnapshotChanged) {
		t.Errorf("stale config apply error = %v", err)
	}
	if _, _, _, ok := s.TargetKeySnapshot(changed); !ok {
		t.Error("stale apply replaced current configuration")
	}
	if _, _, _, ok := s.TargetKeySnapshot(target); ok {
		t.Error("stale stored keys accepted")
	}
}

func TestIssue68SingleKeyHasNoPoolIndex(t *testing.T) {
	target := config.Target{Name: "target", EndpointType: "openai", Endpoint: "https://example.com", APIKey: "one"}
	s, err := NewService(&config.Config{Targets: []config.Target{target}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	revision, statuses, slots, ok := s.TargetKeySnapshot(target)
	if !ok || revision == 0 || statuses != nil || len(slots) != 1 || slots[0] != nil {
		t.Error("single-key target incorrectly advertised a pool index")
	}
}

func TestIssue68RevisionOverflowPermanentlyInvalidatesSnapshots(t *testing.T) {
	for _, operation := range []string{"key-action", "config-update"} {
		t.Run(operation, func(t *testing.T) {
			target := config.Target{Name: "target", EndpointType: "openai", Endpoint: "https://example.com", APIKey: "primary", APIKeys: []string{"second"}}
			cfg := &config.Config{Targets: []config.Target{target}}
			s, err := NewService(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close() })
			oldRevision, _, _, ok := s.TargetKeySnapshot(target)
			if !ok || oldRevision == 0 {
				t.Fatal("missing initial revision")
			}
			s.mu.Lock()
			s.targetRevision = ^uint64(0)
			s.mu.Unlock()
			if operation == "key-action" {
				err = s.SetKeyBlockedIfTargetRevision("target", 1, true, ^uint64(0))
			} else {
				err = s.ApplyConfig(cfg)
			}
			if err != nil {
				t.Fatal(err)
			}
			if revision, _, _, _ := s.TargetKeySnapshot(target); revision != 0 {
				t.Fatalf("overflow revision = %d, want unavailable", revision)
			}
			if err := s.UpdateTargets(cfg.Targets); err != nil {
				t.Fatal(err)
			}
			statusBefore := s.KeyPoolStatus("target")[1].Blocked
			if err := s.SetKeyBlockedIfTargetRevision("target", 1, !statusBefore, oldRevision); !errors.Is(err, ErrTargetSnapshotChanged) {
				t.Errorf("old revision accepted after overflow: %v", err)
			}
			if s.KeyPoolStatus("target")[1].Blocked != statusBefore {
				t.Error("old revision changed a key after overflow")
			}
			if err := s.ApplyConfigIfTargetRevision(cfg, oldRevision); !errors.Is(err, ErrTargetSnapshotChanged) {
				t.Errorf("old config revision accepted after overflow: %v", err)
			}
			if err := s.BlockKey("target", 0); err != nil {
				t.Fatal(err)
			}
			if revision, _, _, _ := s.TargetKeySnapshot(target); revision != 0 {
				t.Fatalf("snapshot revision revived after overflow: %d", revision)
			}
		})
	}
}
