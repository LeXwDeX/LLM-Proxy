package admin

import (
	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/catalog"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/nosql"
	"github.com/LeXwDeX/LLM-Proxy/internal/pricing"
	"github.com/LeXwDeX/LLM-Proxy/internal/proxy"
	"github.com/LeXwDeX/LLM-Proxy/internal/quota"
	"github.com/LeXwDeX/LLM-Proxy/internal/requeststats"
	"github.com/go-chi/chi/v5"
	"log/slog"
	"net/http"
	"sync"
)

// Handler wires administration endpoints.
type Handler struct {
	configManager        *config.Manager
	authStore            *auth.Store
	proxyService         *proxy.Service
	auditStore           *nosql.AuditStore
	userStore            *nosql.UserStore
	clientStore          *nosql.ClientStore
	targetStore          *nosql.TargetStore
	modelCostStore       *nosql.ModelCostStore
	usageStore           *nosql.UsageStore
	requestStatsStore    requeststats.Reader
	modelGroupStore      *nosql.ModelGroupStore
	clientQuotaStore     *nosql.ClientQuotaStore
	jobLevelStore        *nosql.JobLevelStore
	dictStore            *nosql.DictStore
	modelCatalog         *catalog.Catalog
	quotaManager         *quota.Manager        // nil = 配额管理未启用
	costCache            *pricing.PricingCache // nil = 回退到直接 List+ToCostTable
	pricingTemplateStore *nosql.PricingTemplateStore
	sessionManager       *SessionManager
	logger               *slog.Logger

	// router 承载全部 /admin/* 子路由（NewHandler 返回 *Handler 以暴露
	// SetRateFunc/SetOnBillingWrite 注入点，同时满足 http.Handler）。
	router chi.Router

	// configMu serializes all config-snapshot mutations (target CRUD + reload)
	// so concurrent admin writes can't Replace with stale snapshots (lost update).
	configMu sync.Mutex
}

// NewHandler constructs an HTTP router exposing admin endpoints.
func NewHandler(
	manager *config.Manager,
	store *auth.Store,
	service *proxy.Service,
	auditStore *nosql.AuditStore,
	userStore *nosql.UserStore,
	clientStore *nosql.ClientStore,
	targetStore *nosql.TargetStore,
	modelCostStore *nosql.ModelCostStore,
	usageStore *nosql.UsageStore,
	modelCatalog *catalog.Catalog,
	quotaMgr *quota.Manager,
	costCache *pricing.PricingCache,
	sessionMgr *SessionManager,
	logger *slog.Logger,
) *Handler {
	h := &Handler{
		configManager:        manager,
		authStore:            store,
		proxyService:         service,
		auditStore:           auditStore,
		userStore:            userStore,
		clientStore:          clientStore,
		targetStore:          targetStore,
		modelCostStore:       modelCostStore,
		usageStore:           usageStore,
		requestStatsStore:    nosql.NewRequestStatsStore(clientStore.DB()),
		modelGroupStore:      nosql.NewModelGroupStore(clientStore.DB()),
		clientQuotaStore:     nosql.NewClientQuotaStore(clientStore.DB()),
		jobLevelStore:        nosql.NewJobLevelStore(clientStore.DB()),
		dictStore:            nosql.NewDictStore(clientStore.DB()),
		pricingTemplateStore: nosql.NewPricingTemplateStore(clientStore.DB()),
		modelCatalog:         modelCatalog,
		quotaManager:         quotaMgr,
		costCache:            costCache,
		sessionManager:       sessionMgr,
		logger:               logger,
	}

	h.router = h.routes()
	return h
}

// ServeHTTP delegates to the internal chi router.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.router.ServeHTTP(w, r)
}
