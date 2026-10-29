package admin

import (
	"crypto/rand"
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
	"time"
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
	quotaManager         quotaController       // nil = 配额管理未启用
	costCache            *pricing.PricingCache // nil = 回退到直接 List+ToCostTable
	pricingTemplateStore *nosql.PricingTemplateStore
	sessionManager       *SessionManager
	logger               *slog.Logger

	// router 承载全部 /admin/* 子路由（NewHandler 返回 *Handler 以暴露
	// SetRateFunc/SetOnBillingWrite 注入点，同时满足 http.Handler）。
	router chi.Router

	// configMu serializes all config-snapshot mutations (target CRUD + reload)
	// so concurrent admin writes can't Replace with stale snapshots (lost update).
	configMu               sync.Mutex
	keySnapshotSecret      [32]byte
	keySnapshotSecretReady bool

	// keyIDMu 保护 keyIDSecretCache：稳定 key ID 的派生密钥，持久化在 bbolt meta 中，首次读取后缓存。
	keyIDMu          sync.Mutex
	keyIDSecretCache []byte
	// keyIDSecretLoader / authApply 是测试注入点；nil 时分别使用 targetStore.KeyIDSecret 与 authStore。
	keyIDSecretLoader func() ([]byte, error)
	authApply         func(clients []config.Client, targets []config.Target) error
	// clientQuotaEvaluateHook 是测试注入点：客户端写入释放 configMu 之后、配额评估之前调用。
	clientQuotaEvaluateHook func(name string)
	// auditWriteHook 是测试注入点：审计写入之前调用（可用来模拟慢审计存储）。
	auditWriteHook func(nosql.AuditEvent)
	// auditDeferredHook 是测试注入点：configTxn 释放锁之后、取得审计写入参数之前调用。
	auditDeferredHook func(action string)
	// lastAuditAt 是最近一次在 configMu 内分配的审计时间戳（仅在持有 configMu 时访问）。
	lastAuditAt time.Time
	// listETagsLoadedHook 是测试注入点：目标列表读完 ETag 之后、获取 configMu 之前调用。
	listETagsLoadedHook func()
}

// quotaController 是 admin 依赖的额度管理接口（*quota.Manager 实现；测试可注入慢实现）。
type quotaController interface {
	Status(clientName string) quota.QuotaStatus
	Evaluate(clientName string)
	// ScheduleRefresh 登记后台、去抖、按范围的刷新，必须立即返回。
	ScheduleRefresh(scope quota.RefreshScope)
	// NoteConfigChange 在额度相关配置提交之后推进配置代次（让在途校准丢弃旧结果）。
	NoteConfigChange() uint64
	// RefreshStatus 回读后台刷新状态。
	RefreshStatus() quota.RefreshStatus
}

// scheduleQuotaRefresh 在写操作提交后登记额度刷新。调用方不得持有 configMu
// （持锁的 handler 通过 configTxn.refresh 延迟到释放锁之后）。
func (h *Handler) scheduleQuotaRefresh(scope quota.RefreshScope) {
	h.warmCostCache()
	if h.quotaManager != nil {
		h.quotaManager.ScheduleRefresh(scope)
	}
}

// noteQuotaConfigChange 在影响额度判定的写入提交成功之后推进额度配置代次。
func (h *Handler) noteQuotaConfigChange() {
	if h.quotaManager != nil {
		h.quotaManager.NoteConfigChange()
	}
}

// warmCostCache 在 Invalidate 之后于锁外预先重建计价缓存，使 API 路径的下一次
// getResolver 不必回读库；缓存已就绪时是一次原子读。重建失败不缓存，下次照常重试。
func (h *Handler) warmCostCache() {
	if h.costCache != nil {
		_, _ = h.costCache.GetChecked()
	}
}

// quotaRefreshPending 是写响应里的额度刷新提示：配置已提交，受影响客户端的额度
// 计数器在后台刷新（去抖 + 排队 + 单客户端校准），窗口内准入仍使用旧计数器。
const quotaRefreshPending = "pending"

// withQuotaRefresh 给已登记后台额度刷新的写响应加上 "quota_refresh":"pending"
// （未配置额度管理器时不加）。进度回读：GET /admin/metrics 的 quota_refresh。
func (h *Handler) withQuotaRefresh(body map[string]any) map[string]any {
	if h.quotaManager != nil {
		body["quota_refresh"] = quotaRefreshPending
	}
	return body
}

// configTxn 包装 configMu 写 handler：锁内只做配置校验与落库，审计写入与额度
// 刷新登记被推迟到释放 configMu 之后（审计是一次 bbolt 写事务，额度刷新可能
// 很慢，二者都不应拖住其他配置写入与目标列表读取）。审计仍同步写入、不丢失；
// 响应在 handler 返回时才结束，因此客户端收到响应时审计已落库。
type configTxn struct {
	h              *Handler
	r              *http.Request
	audits         []auditArgs
	pendingRefresh []quota.RefreshScope
}

type auditArgs struct {
	action, object, result, detail string
	at                             time.Time
}

// lockConfig 获取 configMu 并返回事务；用法 `txn := h.lockConfig(r); defer txn.Unlock()`。
func (h *Handler) lockConfig(r *http.Request) *configTxn {
	h.configMu.Lock()
	return &configTxn{h: h, r: r}
}

// audit 在持有 configMu 时调用：此刻分配（严格递增的）审计时间戳，写入推迟到锁外。
// 同一把锁下的提交顺序因此等于审计时间戳顺序，与锁外写入的先后无关。
func (t *configTxn) audit(action, object, result, detail string) {
	at := time.Now().UTC()
	if !at.After(t.h.lastAuditAt) {
		at = t.h.lastAuditAt.Add(time.Nanosecond)
	}
	t.h.lastAuditAt = at
	t.audits = append(t.audits, auditArgs{action, object, result, detail, at})
}

func (t *configTxn) refresh(scope quota.RefreshScope) {
	t.pendingRefresh = append(t.pendingRefresh, scope)
}

// Unlock 先释放 configMu，再依次写审计、登记额度刷新。
func (t *configTxn) Unlock() {
	t.h.configMu.Unlock()
	t.h.warmCostCache() // 先于审计：API 路径的下一次计价不应等审计写入
	for _, a := range t.audits {
		if t.h.auditDeferredHook != nil {
			t.h.auditDeferredHook(a.action)
		}
		t.h.recordAuditAt(t.r, a.at, a.action, a.object, a.result, a.detail)
	}
	for _, scope := range t.pendingRefresh {
		t.h.scheduleQuotaRefresh(scope)
	}
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
		costCache:            costCache,
		sessionManager:       sessionMgr,
		logger:               logger,
	}
	if quotaMgr != nil { // 避免把 typed nil 装进接口
		h.quotaManager = quotaMgr
	}
	if _, err := rand.Read(h.keySnapshotSecret[:]); err == nil {
		h.keySnapshotSecretReady = true
	} else if logger != nil {
		logger.Error("unable to initialize target key snapshots")
	}

	h.router = h.routes()
	return h
}

// ServeHTTP delegates to the internal chi router.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.router.ServeHTTP(w, r)
}
