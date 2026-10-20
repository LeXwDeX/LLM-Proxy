package pricing

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

func TestPricingCacheLogsTemplateLoadFailureOnceAndRecovers(t *testing.T) {
	buffer := &lockedBuffer{}
	logger := slog.New(slog.NewTextHandler(buffer, &slog.HandlerOptions{Level: slog.LevelDebug}))

	fail := true
	loadErr := errors.New("bbolt unavailable")
	cache := NewPricingCache(func() ([]PricingTemplate, error) {
		if fail {
			return nil, loadErr
		}
		return []PricingTemplate{}, nil
	}, nil)
	cache.SetLogger(logger)

	cache.Get()
	cache.Get()
	cache.Get()
	if got := buffer.String(); !strings.Contains(got, "pricing template load failed") || !strings.Contains(got, loadErr.Error()) {
		t.Fatalf("first template load failure must be logged with its error, got:\n%s", got)
	}
	if count := strings.Count(buffer.String(), "msg="); count != 1 {
		t.Fatalf("load failure should be logged once while it persists, got %d entries:\n%s", count, buffer.String())
	}

	fail = false
	buffer.Reset()
	cache.Invalidate()
	cache.Get()
	if got := buffer.String(); !strings.Contains(got, "pricing template load recovered") {
		t.Fatalf("recovery must be logged after a reported failure, got:\n%s", got)
	}

	// A later failure is newsworthy again.
	fail = true
	buffer.Reset()
	cache.Invalidate()
	cache.Get()
	if got := buffer.String(); !strings.Contains(got, "pricing template load failed") {
		t.Fatalf("a failure after recovery must be logged again, got:\n%s", got)
	}
}

// A recovery log runs between the generation check and publication in the
// historical implementation, providing a deterministic concurrent invalidation.
func TestPricingCacheInvalidationDuringPublication(t *testing.T) {
	var price atomic.Int64
	price.Store(1)
	cache := NewPricingCache(func() ([]PricingTemplate, error) {
		return []PricingTemplate{{Model: "qwen-max", BaseRates: usage.CostRates{InputPer1MTokens: float64(price.Load())}}}, nil
	}, nil)
	entered, release := make(chan struct{}), make(chan struct{})
	cache.SetLogger(slog.New(publicationBlockingHandler{entered: entered, release: release}))
	cache.templateLoadFailed.Store(true)
	done := make(chan struct{})
	go func() { cache.Get(); close(done) }()
	<-entered
	price.Store(9)
	cache.Invalidate()
	close(release)
	<-done
	rates, ok := cache.Get().RateFor("", "qwen-max", time.Now())
	if !ok || rates.InputPer1MTokens != 9 {
		t.Fatalf("post-invalidation lookup retained stale price: %+v priced=%v", rates, ok)
	}
}

type publicationBlockingHandler struct {
	entered chan struct{}
	release chan struct{}
}

func (h publicationBlockingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h publicationBlockingHandler) Handle(_ context.Context, _ slog.Record) error {
	close(h.entered)
	<-h.release
	return nil
}
func (h publicationBlockingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h publicationBlockingHandler) WithGroup(string) slog.Handler      { return h }

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *lockedBuffer) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf.Reset()
}

func TestPricingCacheLogsProviderLoadFailureSeparately(t *testing.T) {
	buffer := &lockedBuffer{}
	logger := slog.New(slog.NewTextHandler(buffer, &slog.HandlerOptions{Level: slog.LevelDebug}))

	templateFail := true
	providerFail := true
	loadErr := errors.New("template store unavailable")
	providerErr := errors.New("target store unavailable")
	cache := NewTargetPricingCache(func() ([]PricingTemplate, error) {
		if templateFail {
			return nil, loadErr
		}
		return []PricingTemplate{}, nil
	}, nil, func() (map[string]string, error) {
		if providerFail {
			return nil, providerErr
		}
		return map[string]string{"gpt-main": "openai"}, nil
	})
	cache.SetLogger(logger)

	cache.Get()
	cache.Get()
	got := buffer.String()
	if !strings.Contains(got, "pricing template load failed") || !strings.Contains(got, loadErr.Error()) {
		t.Fatalf("template failure must be logged with its error, got:\n%s", got)
	}
	if !strings.Contains(got, "pricing target provider load failed") || !strings.Contains(got, providerErr.Error()) {
		t.Fatalf("provider failure must be logged separately with its error, got:\n%s", got)
	}
	if count := strings.Count(got, "msg="); count != 2 {
		t.Fatalf("each failing source must log exactly once, got %d entries:\n%s", count, got)
	}

	providerFail = false
	buffer.Reset()
	cache.Get()
	got = buffer.String()
	if !strings.Contains(got, "pricing target provider load recovered") {
		t.Fatalf("provider recovery must be logged, got:\n%s", got)
	}
	if strings.Contains(got, "pricing template load recovered") {
		t.Fatalf("template recovery must not be logged while templates still fail, got:\n%s", got)
	}

	templateFail = false
	buffer.Reset()
	cache.Invalidate()
	cache.Get()
	if got := buffer.String(); !strings.Contains(got, "pricing template load recovered") {
		t.Fatalf("template recovery must be logged, got:\n%s", got)
	}
}

func TestPricingCacheGetCheckedReportsSourceErrorsWithoutCachingThem(t *testing.T) {
	for _, failure := range []string{"template", "provider", "both"} {
		t.Run(failure, func(t *testing.T) {
			templateErr, providerErr := errors.New("template failure"), errors.New("provider failure")
			failed := true
			templateCalls, providerCalls := 0, 0
			cache := NewTargetPricingCache(func() ([]PricingTemplate, error) {
				templateCalls++
				if failed && failure != "provider" {
					return nil, templateErr
				}
				return []PricingTemplate{{Model: "qwen-max", BaseRates: usage.CostRates{InputPer1MTokens: 10}}}, nil
			}, nil, func() (map[string]string, error) {
				providerCalls++
				if failed && failure != "template" {
					return nil, providerErr
				}
				return map[string]string{"upstream": "openai"}, nil
			})
			for range 2 {
				resolver, err := cache.GetChecked()
				if resolver == nil || err == nil || errors.Is(err, templateErr) != (failure != "provider") || errors.Is(err, providerErr) != (failure != "template") {
					t.Fatalf("checked lookup must preserve every source failure: resolver=%v error=%v", resolver, err)
				}
			}
			// Existing report consumers still receive a resolver on failure.
			if resolver := cache.Get(); resolver == nil {
				t.Fatal("legacy Get must preserve its degraded resolver contract")
			}
			if templateCalls != 3 || providerCalls != 3 || cache.ptr.Load() != nil {
				t.Fatalf("source failures were cached: templateCalls=%d providerCalls=%d", templateCalls, providerCalls)
			}
			failed = false
			resolver, err := cache.GetChecked()
			if err != nil {
				t.Fatalf("source recovery must not require Invalidate: %v", err)
			}
			rates, ok := resolver.RateFor("upstream", "qwen-max", time.Now())
			if !ok || rates.InputPer1MTokens != 10 {
				t.Fatalf("recovered resolver has wrong price: %+v priced=%v", rates, ok)
			}
			cached, err := cache.GetChecked()
			if err != nil || cached != resolver || templateCalls != 4 || providerCalls != 4 {
				t.Fatalf("successful lookup should cache the reliable resolver: error=%v calls=%d/%d", err, templateCalls, providerCalls)
			}
		})
	}
}
