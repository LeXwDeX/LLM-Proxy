package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata" // Embed IANA zones for static binaries running without system tzdata.

	"github.com/LeXwDeX/LLM-Proxy/internal/admin"
	"github.com/LeXwDeX/LLM-Proxy/internal/admission"
	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/catalog"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/errorlog"
	"github.com/LeXwDeX/LLM-Proxy/internal/logging"
	"github.com/LeXwDeX/LLM-Proxy/internal/nosql"
	"github.com/LeXwDeX/LLM-Proxy/internal/pricing"
	"github.com/LeXwDeX/LLM-Proxy/internal/proxy"
	"github.com/LeXwDeX/LLM-Proxy/internal/quota"
)

func main() { os.Exit(run(os.Args[1:])) }

// stopQuotaManager and closeLogManager are variables only so tests can
// observe the deferred shutdown.
var (
	stopQuotaManager = func(m *quota.Manager) { m.Stop() }
	closeLogManager  = func(m *logging.Manager) error { return m.Close() }
)

// run starts the proxy and returns the process exit status. Every exit path
// returns here instead of calling os.Exit, so the deferred cleanup always
// runs in its established order (LIFO): background work, quota manager,
// database, upstream error log, log manager.
func run(args []string) int {
	flags := flag.NewFlagSet("llms-proxy", flag.ContinueOnError)
	configPath := flags.String("config", "config/config.json", "path to the configuration file")
	if err := flags.Parse(args); err != nil {
		// -h/-help printed the usage on request: not an error.
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	manager := config.NewManager(*configPath)
	cfg, created, err := manager.LoadOrInit()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load config: %v\n", err)
		return 1
	}
	if created {
		fmt.Fprintf(os.Stderr, "default config generated at %s — add targets via admin UI\n", *configPath)
	}

	logLevel := cfg.Logging.Level
	if envLogLevel := strings.TrimSpace(os.Getenv("LOG_LEVEL")); envLogLevel != "" {
		logLevel = envLogLevel
	}

	logManager, err := logging.Setup(logging.Config{
		Level:         logLevel,
		AccessLogPath: cfg.Logging.AccessLog,
		ErrorLogPath:  cfg.Logging.ErrorLog,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to initialize logging: %v\n", err)
		return 1
	}
	defer func() {
		if err := closeLogManager(logManager); err != nil {
			fmt.Fprintf(os.Stderr, "failed to close log manager: %v\n", err)
		}
	}()

	configBind := strings.TrimSpace(cfg.Server.Bind)
	if configBind == "" {
		configBind = "0.0.0.0:8000"
	}
	bindAddr := configBind
	if envBind := strings.TrimSpace(os.Getenv("SERVER_BIND")); envBind != "" {
		bindAddr = envBind
	}

	appLogger := logManager.App()

	// 上游错误日志（旁路 access/error log，仅记 4xx/5xx/net error/panic）。
	// 路径可通过 UPSTREAM_ERROR_LOG_PATH 覆盖，默认 /var/log/llms-proxy/upstream-error.log。
	// 打不开时 Init 内部 slog.Warn 后降级为 noop，不阻断启动。
	errorlog.SetSlogger(appLogger)
	errorlog.Init(strings.TrimSpace(os.Getenv("UPSTREAM_ERROR_LOG_PATH")))
	defer func() {
		if err := errorlog.Close(); err != nil {
			appLogger.Warn("upstream-error log close failed", "error", err)
		}
	}()

	// Open bbolt database.
	dbPath := cfg.DataStore.DBPath
	db, err := nosql.OpenDB(dbPath)
	if err != nil {
		appLogger.Error("failed to open database", "path", dbPath, "error", err)
		return 1
	}
	defer db.Close()

	// Migrate from legacy JSON files if needed.
	if err := nosql.MigrateFromJSON(db, cfg.DataFiles); err != nil {
		appLogger.Warn("json migration encountered errors", "error", err)
	}

	targetsMigrated, err := nosql.MigrateTargetsFromConfig(db, cfg.Targets, appLogger)
	if err != nil {
		appLogger.Error("failed to migrate targets into database", "error", err)
		return 1
	}
	if err := nosql.MigrateUsageAdapterExplicit(db, appLogger); err != nil {
		appLogger.Error("failed to migrate explicit target usage_adapter", "error", err)
		return 1
	}
	if targetsMigrated {
		if err := config.RemoveTargetsFromFile(*configPath); err != nil {
			appLogger.Warn("targets migrated but failed to remove them from config file", "error", err)
		} else {
			appLogger.Info("migrated targets into database and removed legacy config targets")
		}
	}

	// 部署池：与 targets 相同的急切迁移路径（桶空时灌入，成功后从配置文件摘除）。
	if migrated, err := nosql.MigratePoolsFromConfig(db, cfg.Pools); err != nil {
		appLogger.Error("failed to migrate pools into database", "error", err)
		return 1
	} else if migrated {
		if err := config.RemovePoolsFromFile(*configPath); err != nil {
			appLogger.Warn("pools migrated but failed to remove them from config file", "error", err)
		} else {
			appLogger.Info("migrated pools into database and removed legacy config pools")
		}
	}

	usageStore := nosql.NewUsageStore(db)
	requestStatsStore := nosql.NewRequestStatsStore(db)
	usageJournal, err := nosql.NewUsageJournal(usageStore, dbPath+".usage-journal.jsonl", appLogger)
	if err != nil {
		appLogger.Error("failed to initialize usage recovery journal", "path", dbPath+".usage-journal.jsonl", "error", err)
		return 1
	}

	// One-shot backfill of hourly usage aggregation (idempotent via meta marker).
	// Must run BEFORE proxy starts accepting traffic so reads see consistent agg data.
	// It also runs before journal recovery: the receipt watermark compares raw
	// detail with permanent rollups, so those rollups must already exist.
	backfillStart := time.Now()
	if err := nosql.BackfillUsageAgg(db); err != nil {
		appLogger.Error("usage agg backfill failed", "error", err)
		return 1
	}
	if err := nosql.BackfillQuotaUsageAgg(db); err != nil {
		appLogger.Error("quota usage agg backfill failed", "error", err)
		return 1
	}
	appLogger.Info("usage agg backfill done", "duration", time.Since(backfillStart).String())

	// Establish the conservative legacy boundary before any receipt can be
	// written. Undecodable or malformed legacy aggregate evidence does not
	// fail this step: it is logged, and its hour becomes untrusted only when
	// retention may have cleaned that hour. The watermark never passes the
	// hour in which this scan runs.
	// Any other failure (for example a missing bucket or an I/O error) blocks
	// every explicit-identity write until a retry succeeds: journal replay,
	// the asynchronous writer, calibration drains and retention pruning all
	// fail, events stay durable in the recovery journal, and quota admission
	// for clients with queued events fails closed. Later writes retry the
	// migration with a cached exponential backoff instead of rescanning.
	if err := usageStore.EnsureUsageReceipts(); err != nil {
		appLogger.Error("usage receipt migration failed; explicit usage writes stay queued in the recovery journal until a retry succeeds", "error", err)
	}
	// Warm the legacy-row proof cache before the writer starts, so the first
	// usage batch does not parse the whole history inside a write transaction.
	warmStarted := time.Now()
	if rows, err := nosql.WarmUsageProofScanCache(db); err != nil {
		appLogger.Warn("usage proof cache warm-up failed; first usage writes may be slow", "error", err)
	} else if rows > 0 {
		appLogger.Info("usage proof cache warmed", "rows", rows, "duration", time.Since(warmStarted).String())
	}

	if recoveredClients, err := usageJournal.Replay(); err != nil {
		// Individual records that keep failing transiently must not take the
		// whole proxy down: they stay queued in the journal and every retry
		// tick reports the failure until the underlying cause is resolved.
		// Rows the ledger permanently rejects are moved to the journal's
		// ".rejected" sidecar (logged at ERROR) instead of staying queued.
		appLogger.Error("usage journal recovery left events pending; continuing startup",
			"error", err)
	} else if len(recoveredClients) > 0 {
		appLogger.Info("recovered pending usage events before startup", "clients", len(recoveredClients))
	}

	// Create all bbolt-backed stores.
	clientStore := nosql.NewClientStore(db)
	modelGroupStore := nosql.NewModelGroupStore(db)
	clientQuotaStore := nosql.NewClientQuotaStore(db)
	jobLevelStore := nosql.NewJobLevelStore(db)
	targetStore := nosql.NewTargetStore(db, appLogger)
	poolStore := nosql.NewPoolStore(db)
	modelCostStore := nosql.NewModelCostStore(db)
	pricingTemplateStore := nosql.NewPricingTemplateStore(db)
	userStore := nosql.NewUserStore(db)
	auditStore := nosql.NewAuditStore(db)

	// Migrate old "endpoint_type:model" keys to model-only keys (idempotent).
	if err := modelCostStore.MigrateKeys(); err != nil {
		appLogger.Warn("failed to migrate model cost keys", "error", err)
	}

	// Migrate model_costs into flat template-0 pricing templates (idempotent).
	// Fail-fast: billing reads exclusively from pricing_templates, so a failed
	// migration would silently reprice every model at catalog defaults.
	if err := nosql.MigrateModelCostsToPricingTemplates(db); err != nil {
		appLogger.Error("failed to migrate model costs to pricing templates", "error", err)
		return 1
	}

	// Migrate legacy B1/B2 billing data (billing_templates + time_segments →
	// pricing_templates; consumer groups → model_groups; per-target model_costs
	// → (target, model) template 0). Idempotent via meta marker; fail-fast for
	// the same reprice-at-defaults reason as above.
	if err := nosql.MigrateBillingData(db, appLogger); err != nil {
		appLogger.Error("failed to migrate legacy billing data", "error", err)
		return 1
	}

	// 额度体系 v2 一次性迁移（幂等，meta 标记）：v1 配额快照落盘（数据目录）→
	// 职级字典升级为结构化系数记录 → 客户端 plan 限额覆盖作废（系数默认 1）。
	// 快照写失败即中止，不改写任何数据。
	if err := nosql.MigrateQuotaV2(db, filepath.Dir(dbPath), appLogger); err != nil {
		appLogger.Error("failed to migrate quota v2", "error", err)
		return 1
	}

	sessionManager := admin.NewSessionManager(cfg.AdminSession, appLogger)

	clients, err := clientStore.List()
	if err != nil {
		appLogger.Error("failed to load clients from database",
			"error", err,
		)
		return 1
	}
	targets, err := targetStore.List()
	if err != nil {
		appLogger.Error("failed to load targets from database", "error", err)
		return 1
	}
	cfg.Targets = targets
	pools, err := poolStore.List()
	if err != nil {
		appLogger.Error("failed to load pools from database", "error", err)
		return 1
	}
	cfg.Pools = pools
	manager.Replace(cfg)

	appLogger.Info("configuration loaded",
		"config_path", *configPath,
		"config_bind", configBind,
		"effective_bind", bindAddr,
		"config_log_level", cfg.Logging.Level,
		"effective_log_level", logLevel,
		"targets", len(cfg.Targets),
		"clients", len(clients),
		"db_path", dbPath,
	)

	cfg.TraceStore = configureTraceFromEnvironment(cfg.TraceStore, appLogger)

	authStore := auth.NewStore()
	// allowed_pools → 成员 target 名展开（授权一池即授权全部成员节点）。
	if err := authStore.LoadFromConfigWithPools(clients, targets); err != nil {
		appLogger.Error("failed to initialize auth store", "error", err)
		return 1
	}

	// Seed default admin user if the user store is empty.
	existingAdmins, err := userStore.List()
	if err != nil {
		appLogger.Error("failed to load admin users", "error", err)
		return 1
	}
	if len(existingAdmins) == 0 {
		// Determine initial password: prefer ADMIN_PASSWORD env var, fallback to random.
		initialPassword := strings.TrimSpace(os.Getenv("ADMIN_PASSWORD"))
		passwordSource := "environment variable ADMIN_PASSWORD"
		if initialPassword == "" {
			randomBytes := make([]byte, 12)
			if _, err := rand.Read(randomBytes); err != nil {
				appLogger.Error("failed to generate random admin password", "error", err)
				return 1
			}
			initialPassword = hex.EncodeToString(randomBytes)
			passwordSource = "random generation"
		}
		passwordHash, err := admin.HashPasswordWithRandomSalt(initialPassword)
		if err != nil {
			appLogger.Error("failed to hash admin password", "error", err)
			return 1
		}
		seedErr := userStore.SeedDefaultUser(config.AdminUser{
			Username:     admin.DefaultAdminUsername,
			PasswordHash: passwordHash,
			Role:         "admin",
		})
		if seedErr != nil {
			appLogger.Warn("failed to seed default admin user", "error", seedErr)
		} else {
			appLogger.Info("seeded default admin user",
				"username", "admin",
				"password_source", passwordSource,
			)
			if passwordSource != "environment variable ADMIN_PASSWORD" {
				// Only print password to stderr when randomly generated (one-time visibility).
				fmt.Fprintf(os.Stderr, "\n"+
					"============================================================\n"+
					"  INITIAL ADMIN PASSWORD (shown only once)\n"+
					"  Username: admin\n"+
					"  Password: %s\n"+
					"  Or set ADMIN_PASSWORD env var before first start.\n"+
					"============================================================\n\n",
					initialPassword)
			}
		}
	}

	// 测试实例固定 admin 密码（议题 #69）：config 设置 admin_password_fix 时，
	// 每次启动都作废旧密码并恢复为固定值，保证重启/重建后约定凭据可用。
	// 生产 config 不得设置该键；未设置时此处为零开销空操作。
	if fixed := strings.TrimSpace(cfg.AdminPasswordFix); fixed != "" {
		if err := admin.ApplyFixedAdminPassword(userStore, "admin", fixed); err != nil {
			appLogger.Error("failed to apply fixed admin password (admin_password_fix)", "error", err)
			return 1
		}
		appLogger.Warn("admin password reset from config admin_password_fix",
			"username", "admin",
			"note", "admin_password_fix is set; intended for test instances only")
	}

	modelCatalog, err := catalog.New()
	if err != nil {
		appLogger.Warn("failed to load model catalog, continuing without defaults", "error", err)
	}

	configureObservationBudgetFromEnvironment(appLogger)
	proxyService, err := proxy.NewService(cfg, appLogger)
	if err != nil {
		appLogger.Error("failed to initialize proxy service", "error", err)
		return 1
	}

	// Usage is synced to the recovery journal before the response ends and
	// committed to bbolt in the background; completion statistics are queued.
	// Neither bbolt commit delays the end of a (streaming) response.
	persistence, err := startUsagePersistence(usageJournal, requestStatsStore, appLogger)
	if err != nil {
		appLogger.Error("failed to start usage persistence writers", "error", err)
		return 1
	}
	proxyService.SetUsageRecorder(usageJournal)
	proxyService.SetRequestStatsRecorder(persistence.stats)

	// Initialize client quota manager。
	// 周期性评估所有配置了 quota 的 client；超限时在请求准入阶段返回 429，并在 SSE 流期间触发 TCP RST 中断。
	// 构造失败时降级为 nil Manager，s.quotaManager == nil 跳过检查，不影响主链路。
	pricingCache := pricing.NewTargetPricingCache(func() ([]pricing.PricingTemplate, error) {
		return pricingTemplateStore.List()
	}, modelCatalog, func() (map[string]string, error) {
		targets, err := targetStore.List()
		if err != nil {
			return nil, err
		}
		providers := make(map[string]string, len(targets))
		for _, target := range targets {
			providers[target.Name] = target.EndpointType
		}
		return providers, nil
	})
	pricingCache.SetLogger(appLogger)
	quotaMgr, quotaMgrErr := quota.New(quota.Options{
		Catalog:          modelCatalog,
		CostStore:        modelCostStore,
		CostCache:        pricingCache,
		UsageStore:       usageStore,
		UsageJournal:     usageJournal,
		ClientStore:      clientStore,
		ModelGroupStore:  modelGroupStore,
		ClientQuotaStore: clientQuotaStore,
		// 额度体系 v2：职级系数是个人维度额度的兜底来源（未绑职级 = 拒绝）。
		JobLevelStore: jobLevelStore,
		Logger:        appLogger,
	})
	if quotaMgrErr != nil {
		appLogger.Warn("failed to initialize quota manager, quota checks disabled", "error", quotaMgrErr)
		quotaMgr = nil
	}
	if quotaMgr != nil {
		quotaCtx, quotaCancel := context.WithCancel(context.Background())
		defer quotaCancel()
		if err := quotaMgr.Start(quotaCtx); err != nil {
			appLogger.Warn("quota manager start failed, quota checks disabled", "error", err)
		} else {
			proxyService.SetQuotaManager(quotaMgr)
			defer stopQuotaManager(quotaMgr)
		}
	}

	// WP-2: Initialize admission manager for upstream capacity control.
	// Token bucket + AIMD adaptive rate + bounded queue per target.
	// m == nil safe: if disabled, all SetAdmissionManager(nil) calls skip checks.
	admissionMgr := admission.NewManager(appLogger)
	// SetAdmissionManager registers every published target with its merged key
	// count and current active keys, and binds key-pool callbacks. Re-running
	// InitTarget here would reset an all-exhausted pool to full capacity.
	proxyService.SetAdmissionManager(admissionMgr)
	appLogger.Info("admission manager initialized", "targets", len(cfg.Targets))

	portal := admin.NewPortal(userStore, sessionManager, auditStore, appLogger)
	adminHandler := admin.NewHandler(manager, authStore, proxyService, auditStore, userStore, clientStore, targetStore, modelCostStore, usageStore, modelCatalog, quotaMgr, pricingCache, sessionManager, appLogger)
	router := newRouter(routeDependencies{
		Sessions: sessionManager, Clients: authStore, Portal: portal,
		Admin: adminHandler, Proxy: proxyService,
		AppLogger: appLogger, AccessLogger: logManager.Access(),
	})

	server := &http.Server{
		Addr:              bindAddr,
		Handler:           router,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       time.Duration(cfg.Server.RequestTimeoutSeconds) * time.Second,
		// WriteTimeout 必须为 0：长流式（SSE）响应的生命周期由 per-request context
		// （service.setRequestTimeout，热加载可调）控制。若在此设置与业务超时同值的
		// WriteTimeout，其从读完请求头即起算且早于业务超时触发，会在临界点直接 RST
		// 连接，客户端表现为流中途断裂而非结构化错误。
		WriteTimeout: 0,
		IdleTimeout:  90 * time.Second,
	}

	// 优雅关闭：SIGINT/SIGTERM 时先停止接收新连接并排空在途请求，再由 main 的
	// defer 链依次关闭 quota/tracestore/bbolt/日志，避免部署时强杀在途流式请求
	// 且 defer 清理全部不执行（bbolt 未 Close、日志缓冲丢失）。
	// 宽限期默认 30s，可用 SHUTDOWN_GRACE_SECONDS 覆盖（需同步调大容器 stop 超时）。
	shutdownGrace := shutdownGraceFromEnvironment(appLogger)

	stopCtx, stopCancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopCancel()
	stopUsageIndexMigration := startUsageClientIndexMigration(stopCtx, db, appLogger, nosql.MigrateUsageClientIndex)
	defer stopUsageIndexMigration()
	usageJournal.Start(stopCtx, func(clientNames []string) {
		if quotaMgr == nil {
			return
		}
		for _, clientName := range clientNames {
			quotaMgr.Evaluate(clientName)
		}
	})

	startUsageRetention(stopCtx, usageStore, appLogger)
	startRequestStatsRetention(stopCtx, requestStatsStore, appLogger)
	startMmapHeadroomMonitor(stopCtx, db, appLogger)

	serveErr := serveAndDrain(stopCtx, server, shutdownGrace, appLogger, proxyService.DrainResponsesWebsockets)
	// Shutdown order, the same on both paths: the HTTP server stopped
	// accepting and drained handlers and hijacked WebSocket turns
	// (serveAndDrain); the proxy service closes, canceling remaining turns
	// and waiting for their usage recording; then the usage and statistics
	// queues are drained and the journal replayed; the database closes last.
	finishShutdown(appLogger, func() {
		stopCancel()
		stopUsageIndexMigration()
	}, proxyService.Close, func() {
		persistence.drain(func(recoveredClients []string) {
			if quotaMgr == nil {
				return
			}
			for _, clientName := range recoveredClients {
				quotaMgr.Evaluate(clientName)
			}
		})
	})
	if serveErr != nil {
		// The deferred cleanup closes the database after the drain above.
		return 1
	}

	appLogger.Info("server stopped")
	return 0
}

// minUsageRetentionDays keeps raw usage events for the whole monthly quota
// window plus boundary hours: exact-pricing calibration rejects history whose
// raw events were pruned while their rollups remain.
const minUsageRetentionDays = 35

// usageRetentionDays parses USAGE_RETENTION_DAYS (default 90, 0 = disabled)
// and returns a warning when the value was invalid or raised to the minimum.
func usageRetentionDays(raw string) (int, string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 90, ""
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v < 0 {
		return 90, "invalid USAGE_RETENTION_DAYS, using default"
	}
	if v > 0 && v < minUsageRetentionDays {
		return minUsageRetentionDays, "USAGE_RETENTION_DAYS is shorter than the monthly quota window; using the minimum"
	}
	return v, ""
}
