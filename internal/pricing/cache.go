package pricing

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/LeXwDeX/LLM-Proxy/internal/catalog"
)

// PricingCache caches a *Resolver behind an atomic.Pointer. load supplies the
// current templates (e.g. from a bbolt store); catalog supplies catalog-default
// fallback. Callers that mutate templates call Invalidate() so the next Get()
// rebuilds lazily.
type PricingCache struct {
	catalog             *catalog.Catalog
	load                func() ([]PricingTemplate, error)
	loadTargetProviders func() (map[string]string, error)
	ptr                 atomic.Pointer[Resolver]
	gen                 atomic.Uint64 // bumped by Invalidate; guards against stale lazy-rebuild Store
	publicationMu       sync.Mutex    // serializes generation check + publication with Invalidate

	logger             atomic.Pointer[slog.Logger]
	templateLoadFailed atomic.Bool // a template load failure has been reported and not yet recovered
	providerLoadFailed atomic.Bool // a target provider load failure has been reported and not yet recovered
}

// NewPricingCache constructs a cache. load and cat may both be nil (Get builds
// a Resolver from empty templates and no catalog fallback).
func NewPricingCache(load func() ([]PricingTemplate, error), cat *catalog.Catalog) *PricingCache {
	return &PricingCache{load: load, catalog: cat}
}

// NewTargetPricingCache also binds GPT catalog fallback to each target's provider.
// Invalidate after changing target configuration as well as pricing templates.
func NewTargetPricingCache(load func() ([]PricingTemplate, error), cat *catalog.Catalog, targets func() (map[string]string, error)) *PricingCache {
	c := NewPricingCache(load, cat)
	c.loadTargetProviders = targets
	return c
}

// SetLogger makes load failures observable: while a source keeps failing,
// Get() silently serves degraded prices, so the first failure of each source
// is logged once and its recovery is logged when it starts working again.
func (c *PricingCache) SetLogger(logger *slog.Logger) {
	c.logger.Store(logger)
}

// Get returns the cached *Resolver (read-only). On first access it lazily
// builds the resolver via load + NewResolver + Store. If load errors, empty
// templates are used (never panics, never blocks the caller).
func (c *PricingCache) Get() *Resolver {
	r, _ := c.GetChecked()
	return r
}

// GetChecked returns a resolver and any source-load failure. Callers making
// authoritative accounting decisions must reject degraded prices on error.
// Get retains its historical fallback behavior for estimated usage reports.
// Failed loads are never cached, so the next call can observe source recovery.
func (c *PricingCache) GetChecked() (*Resolver, error) {
	if p := c.ptr.Load(); p != nil {
		return p, nil
	}

	// Lazy build.
	gen := c.gen.Load()
	var templates []PricingTemplate
	var loadErr error
	if c.load != nil {
		var err error
		templates, err = c.load()
		if err != nil {
			c.logTemplateLoadFailure(err)
			loadErr = fmt.Errorf("load pricing templates: %w", err)
		}
	}
	r := NewResolver(templates, c.catalog)
	if c.loadTargetProviders != nil {
		providers, err := c.loadTargetProviders()
		if err != nil {
			c.logProviderLoadFailure(err)
			// Do not cache or guess GPT pricing without provider identity.
			r.gptCatalogUnavailable = true
			return r, errors.Join(loadErr, fmt.Errorf("load pricing target providers: %w", err))
		}
		c.logProviderLoadRecovery()
		r.targetProviders = make(map[string]string, len(providers))
		for target, provider := range providers {
			r.targetProviders[strings.ToLower(strings.TrimSpace(target))] = strings.ToLower(strings.TrimSpace(provider))
		}
	}
	// 仅当重建期间未发生 Invalidate 才写回缓存：否则本次 load 读到的可能是
	// 失效前的旧数据，无条件 Store 会把这次失效覆盖掉且无 TTL，陈旧价格一直
	// 保留到下次变更或重启。本次调用仍返回刚构建的 resolver（可能略旧，
	// 下次 Get 会重建）。
	c.publicationMu.Lock()
	cached := loadErr == nil && c.gen.Load() == gen
	if cached {
		c.ptr.Store(r)
	}
	c.publicationMu.Unlock()
	// Logging may block or call arbitrary handlers; keep it outside the
	// publication lock so invalidation never waits for a log sink.
	if cached {
		c.logTemplateLoadRecovery()
	}
	return r, loadErr
}

// Invalidate clears the cached entry; the next Get() rebuilds it.
func (c *PricingCache) Invalidate() {
	c.publicationMu.Lock()
	defer c.publicationMu.Unlock()
	c.gen.Add(1) // 先推进代数再清指针，使并发 Get 的重建结果无法覆盖本次失效
	c.ptr.Store(nil)
}

// Failed loads are not cached, so every Get() retries them until they
// succeed; the once-only flags only keep the repeated failures quiet.
func (c *PricingCache) logTemplateLoadFailure(err error) {
	if !c.templateLoadFailed.Swap(true) {
		if logger := c.logger.Load(); logger != nil {
			logger.Warn("pricing template load failed; serving catalog default prices until the load succeeds", "error", err)
		}
	}
}

func (c *PricingCache) logProviderLoadFailure(err error) {
	if !c.providerLoadFailed.Swap(true) {
		if logger := c.logger.Load(); logger != nil {
			logger.Warn("pricing target provider load failed; scoped catalog pricing disabled until the load succeeds", "error", err)
		}
	}
}

func (c *PricingCache) logTemplateLoadRecovery() {
	if !c.templateLoadFailed.CompareAndSwap(true, false) {
		return
	}
	if logger := c.logger.Load(); logger != nil {
		logger.Info("pricing template load recovered; configured prices are in effect again")
	}
}

func (c *PricingCache) logProviderLoadRecovery() {
	if !c.providerLoadFailed.CompareAndSwap(true, false) {
		return
	}
	if logger := c.logger.Load(); logger != nil {
		logger.Info("pricing target provider load recovered; scoped catalog pricing enabled")
	}
}
