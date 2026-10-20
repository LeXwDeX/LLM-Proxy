package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"unicode"
)

const (
	EndpointTypeAzureOpenAI  = "azure_openai"
	EndpointTypeOpenAI       = "openai"
	EndpointTypeClaude       = "claude"
	EndpointTypeGemini       = "gemini"
	EndpointTypeOpenAIImage  = "openai_image"  // OpenAI 图片生成/编辑（独立终态 URL）
	EndpointTypeDualProtocol = "dual_protocol" // 双协议兼容（OpenAI + Anthropic，按路径自动识别，prefix 由 target 配置）
)

const (
	ImageOperationGenerations = "generations"
	ImageOperationEdits       = "edits"
	ImageOperationVariations  = "variations"
)

const (
	UsageAdapterAuto       = "auto" // Legacy migration input only; not a valid explicit selection.
	UsageAdapterStandard   = "standard"
	UsageAdapterQwen       = "qwen"
	UsageAdapterGLM        = "glm"
	UsageAdapterDeepSeek   = "deepseek"
	UsageAdapterKimi       = "kimi"
	UsageAdapterOpenRouter = "openrouter"
	UsageAdapterMinimax    = "minimax"
)

// NormalizeUsageAdapter trims and case-normalizes the explicit selection.
// Empty remains empty; there is no default selection.
func NormalizeUsageAdapter(adapter string) string {
	return strings.ToLower(strings.TrimSpace(adapter))
}

// IsValidUsageAdapter reports whether adapter is a supported target-level
// usage normalization policy.
func IsValidUsageAdapter(adapter string) bool {
	return enumContains(usageAdapterOptions, NormalizeUsageAdapter(adapter))
}

// ValidEndpointTypes lists all supported endpoint types.
//
// 从 endpoint_type.go 的 endpointTypes 元数据派生，保持单一信息源。
// 新增类型请在 endpoint_type.go 的 endpointTypes 切片中追加，无需修改此处。
var ValidEndpointTypes = func() []string {
	metas := AllEndpointTypeMetas()
	out := make([]string, 0, len(metas))
	for _, m := range metas {
		out = append(out, m.Code)
	}
	return out
}()

// NormalizeEndpointType returns a canonical endpoint type; empty defaults to azure_openai.
// Alias resolution is driven by EndpointTypeMeta.Aliases in endpoint_type.go (single source of truth).
func NormalizeEndpointType(t string) string {
	t = strings.ToLower(strings.TrimSpace(t))
	if t == "" {
		return EndpointTypeAzureOpenAI
	}
	// Check aliases declared in endpointTypes metadata.
	if canonical, ok := endpointTypeFromAlias(t); ok {
		return canonical
	}
	return t
}

// NormalizeTargetForCompatibility returns a target with legacy endpoint fields normalized.
func NormalizeTargetForCompatibility(target Target) Target {
	rawType := strings.ToLower(strings.TrimSpace(target.EndpointType))
	target.EndpointType = NormalizeEndpointType(target.EndpointType)
	if target.EndpointType != EndpointTypeOpenAI {
		target.PreferWebSocket = false
	}
	target.ImageOperation = NormalizeImageOperation(target.ImageOperation)
	// Runtime auth selection has always matched "bearer" exactly; only trim
	// here. Case normalization happens on admin writes, never on stored data.
	target.AuthMode = strings.TrimSpace(target.AuthMode)
	switch rawType {
	case "deepseek":
		if strings.TrimSpace(target.AnthropicPrefix) == "" {
			target.AnthropicPrefix = "/anthropic"
		}
	case "bailian", "bailian_api":
		if strings.TrimSpace(target.OpenAIPrefix) == "" {
			target.OpenAIPrefix = "/compatible-mode"
		}
		if strings.TrimSpace(target.AnthropicPrefix) == "" {
			target.AnthropicPrefix = "/apps/anthropic"
		}
		target.SupportsResponses = true
	case "wangsu_openai_image":
		target.ImageOperation = ImageOperationGenerations
	case "wangsu_openai_image_edit":
		target.ImageOperation = ImageOperationEdits
	}
	if target.EndpointType == EndpointTypeOpenAIImage && target.ImageOperation == "" {
		target.ImageOperation = InferImageOperationFromEndpoint(target.Endpoint)
	}
	return target
}

// NormalizeImageOperation returns a canonical image operation value.
func NormalizeImageOperation(op string) string {
	return strings.ToLower(strings.TrimSpace(op))
}

// InferImageOperationFromEndpoint infers the image operation for legacy openai_image targets.
func InferImageOperationFromEndpoint(endpoint string) string {
	endpoint = strings.ToLower(endpoint)
	if strings.Contains(endpoint, "variation") {
		return ImageOperationVariations
	}
	if strings.Contains(endpoint, "edit") {
		return ImageOperationEdits
	}
	return ImageOperationGenerations
}

// IsValidEndpointType reports whether t is a recognised endpoint type.
func IsValidEndpointType(t string) bool {
	_, ok := EndpointTypeMetaOf(t)
	return ok
}

// DataStore configures the bbolt embedded database.
type DataStore struct {
	DBPath string `json:"db_path"`
}

// Config is the top-level configuration model matching config/config.json.
type Config struct {
	Server       ServerConfig       `json:"server"`
	Targets      []Target           `json:"targets"`
	Pools        []DeploymentPool   `json:"pools,omitempty"` // 部署池（迁移到 bbolt 后由 store 提供）
	DataStore    DataStore          `json:"data_store"`
	DataFiles    DataFiles          `json:"data_files,omitempty"` // legacy, used for migration only
	AdminSession AdminSessionConfig `json:"admin_session"`
	// AdminPasswordFix 非空时，每次启动强制把 admin 用户密码重置为该值
	// （测试实例固定密码，值只存服务器本地配置，生产禁止设置）。
	AdminPasswordFix string           `json:"admin_password_fix,omitempty"`
	Logging          LoggingConfig    `json:"logging"`
	TraceStore       TraceStoreConfig `json:"trace_store,omitempty"`
}

// TraceStoreConfig controls the DEBUG mode trace store.
// 仅当 Enabled=true 时启用，生产环境应保持 false 以避免性能开销。
type TraceStoreConfig struct {
	Enabled        bool   `json:"enabled"`          // 是否启用（仅 DEBUG 时开启）
	RingBufferSize int    `json:"ring_buffer_size"` // 内存 Ring Buffer 容量（条数）
	MaxBodySize    int    `json:"max_body_size"`    // 单个 body 最大字节数
	DiskPath       string `json:"disk_path"`        // 磁盘存储路径
	DiskMaxSizeMB  int    `json:"disk_max_size_mb"` // 单个日志文件最大 MB
	DiskMaxBackups int    `json:"disk_max_backups"` // 保留的旧日志文件数
	DiskTTLHours   int    `json:"disk_ttl_hours"`   // 磁盘记录 TTL（小时）
	ChannelBuffer  int    `json:"channel_buffer"`   // 异步写入 channel buffer
}

// DataFiles contains paths to file-backed NoSQL data.
type DataFiles struct {
	ClientsFile     string `json:"clients_file"`
	ModelCostsFile  string `json:"model_costs_file"`
	UsageEventsFile string `json:"usage_events_file"`
	AdminUsersFile  string `json:"admin_users_file"`
	AdminAuditFile  string `json:"admin_audit_file"`
}

// AdminSessionConfig controls the admin login session.
type AdminSessionConfig struct {
	CookieName        string `json:"cookie_name"`
	Secret            string `json:"secret"`
	TTLSeconds        int    `json:"ttl_seconds"`
	SlidingExpiration bool   `json:"sliding_expiration"`
	SecureCookie      bool   `json:"secure_cookie"`
}

// AdminUser describes one admin account.
type AdminUser struct {
	Username     string `json:"username"`
	PasswordHash string `json:"password_hash"`
	Role         string `json:"role"`
	Disabled     bool   `json:"disabled"`
	UpdatedAt    string `json:"updated_at,omitempty"`
}

// ServerConfig controls the HTTP server behaviour.
type ServerConfig struct {
	Bind                  string `json:"bind"`
	RequestTimeoutSeconds int    `json:"request_timeout_seconds"`
	// QuietPeriodSeconds 失败静默基线冷却秒数（S4 熔断 cooldown 基线，议题 #18 起可配）。
	// 0/负值使用内置默认 60s。half-open 探测失败的指数延长以此值为基数。
	QuietPeriodSeconds int `json:"quiet_period_seconds,omitempty"`
}

// ModelMapping maps an upstream model name to zero or more downstream aliases.
//
// Model and alias lookup is case-insensitive after trimming whitespace. When a
// request resolves to this mapping, the configured upstream string is forwarded
// byte-for-byte, including its original case.
//
// Fallback supports a comma-separated list of aliases, e.g. "gpt-4,gpt4-turbo"
// lets both names resolve to the same upstream. Empty entries between commas
// are ignored. The local /v1/models listing surfaces every alias plus the
// upstream name as independent, equally-ranked entries — no mapping relation
// is exposed to clients.
type ModelMapping struct {
	Upstream string `json:"upstream"`           // 必填，上游真实模型名
	Fallback string `json:"fallback,omitempty"` // 可选，逗号分隔的下游别名列表
	Paused   bool   `json:"paused,omitempty"`   // 可选，暂停后该模型（含别名）不可被请求；与负载均衡/静默无关，仅表达可用性
}

// SplitFallbackAliases splits a comma-separated fallback string into trimmed,
// non-empty alias names. Returns nil when fallback is empty.
func SplitFallbackAliases(fallback string) []string {
	fallback = strings.TrimSpace(fallback)
	if fallback == "" {
		return nil
	}
	parts := strings.Split(fallback, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Target represents one upstream endpoint (Azure OpenAI, OpenAI, Claude, Gemini, OpenAI Image, etc.).
type Target struct {
	Name               string         `json:"name"`
	EndpointType       string         `json:"endpoint_type,omitempty"` // azure_openai | openai | claude | gemini | openai_image | dual_protocol; default azure_openai
	Endpoint           string         `json:"endpoint"`
	ResourcePathPrefix string         `json:"resource_path_prefix,omitempty"` // deprecated: 保留用于旧数据反序列化，运行时不再使用
	APIKey             string         `json:"api_key"`
	APIKeys            []string       `json:"api_keys,omitempty"`       // 额外 key 池（与 api_key 合并为有序池）
	KeyResetTime       string         `json:"key_reset_time,omitempty"` // 额度重置时间点（CST），格式 "23"/"monthly:23"（每月23号）或 "2006-01-02"/"2006-01-02 15:04"
	ProviderClass      string         `json:"provider_class,omitempty"` // subscription | pay_as_you_go; 影响限流/超额/额度耗尽的处理策略
	UsageAdapter       string         `json:"usage_adapter,omitempty"`  // standard | qwen | glm | deepseek | kimi; legacy empty/auto allowed only on file import
	Paused             bool           `json:"paused,omitempty"`
	Pool               string         `json:"pool,omitempty"`            // 部署池成员归属，至多一池；""=未入池
	MaxConcurrency     int            `json:"max_concurrency,omitempty"` // S1 惰性（S2 激活），0=未设置
	AllowBearer        bool           `json:"allow_bearer_passthrough"`
	AuthMode           string         `json:"auth_mode,omitempty"` // "bearer" | "" (default: x-api-key for claude types)
	ImageOperation     string         `json:"image_operation,omitempty"`
	ModelMappings      []ModelMapping `json:"model_mappings,omitempty"`
	ModelPrefix        string         `json:"model_prefix,omitempty"`
	// dual_protocol 专用字段
	OpenAIPrefix      string `json:"openai_prefix,omitempty"`      // OpenAI 路径前缀，如 "/compatible-mode"
	AnthropicPrefix   string `json:"anthropic_prefix,omitempty"`   // Anthropic 路径前缀，如 "/apps/anthropic"
	SupportsResponses bool   `json:"supports_responses,omitempty"` // 是否支持 /v1/responses
	PreferWebSocket   bool   `json:"prefer_websocket,omitempty"`   // openai: prefer Responses WebSocket before sending a generation

	// RequestStreamUsageIfMissing applies to actual OpenAI Chat requests on any compatible target.
	RequestStreamUsageIfMissing bool `json:"request_stream_usage_if_missing,omitempty"`

	// CustomHeaders 自定义 HTTP 请求头，每个请求注入。
	// 受安全黑名单保护，不可覆盖代理层认证头。
	CustomHeaders map[string]string `json:"custom_headers,omitempty"`
	// CustomBody 自定义请求体 JSON 顶层字段，每个请求合并到 body。
	// 受安全黑名单保护，不可覆盖 model/messages/stream/n。
	CustomBody map[string]any `json:"custom_body,omitempty"`
	// Immutable file-input provenance for field-level migration auditing. It
	// is not serialized and is never used for runtime adapter selection.
	usageAdapterInput *usageAdapterInput
}

// PoolQuota 是客户端在一个部署池（渠道商）上的分层限额。
//
// Deprecated: 池分层限额已退役（额度体系 v2），字段仅保留用于旧数据反序列化
// 与迁移快照；额度评估与 UI 不再读写。部署池只保留路由/授权职能。
type PoolQuota struct {
	Pool       string  `json:"pool"`
	DailyUSD   float64 `json:"daily_usd,omitempty"`
	WeeklyUSD  float64 `json:"weekly_usd,omitempty"`
	MonthlyUSD float64 `json:"monthly_usd,omitempty"`
}

// Client describes a consumer and its access rights.
// Per-client model-group quota selections live in a dedicated nosql bucket
// (ClientQuotaStore); they are not part of the client record.
//
// 额度体系 v2：个人日/周/月额度与池分层限额已退役（字段保留用于旧数据
// 反序列化与迁移快照，评估不再读取）。个人维度限额由职级系数兜底：
// 客户端绑定 JobLevel（职级名），有效额度 = 模型组基数 × 职级系数 ×
// 个人系数；模型原价按组系数折算已用（见 internal/quota）。
type Client struct {
	Name            string      `json:"name"`
	AccessKey       string      `json:"access_key"`
	AllowedTargets  []string    `json:"allowed_targets"`
	AllowedPools    []string    `json:"allowed_pools,omitempty"` // 授权池列表（授权一池即授权全部成员节点）
	IPAddress       string      `json:"ip_address,omitempty"`    // Optional IPv4/CIDR source allowlist, comma-separated after storage normalization.
	JobLevel        string      `json:"job_level,omitempty"`
	ProjectGroup    string      `json:"project_group,omitempty"`
	QuotaDailyUSD   float64     `json:"quota_daily_usd,omitempty"`   // Deprecated: 额度 v2 起不再评估
	QuotaWeeklyUSD  float64     `json:"quota_weekly_usd,omitempty"`  // Deprecated: 额度 v2 起不再评估
	QuotaMonthlyUSD float64     `json:"quota_monthly_usd,omitempty"` // Deprecated: 额度 v2 起不再评估
	PoolQuotas      []PoolQuota `json:"pool_quotas,omitempty"`       // Deprecated: 池分层限额已退役
}

// QuotaGroup is one client's plan entry: a read-only application of a global
// model group plus a personal coefficient. The group's base limits and cost
// coefficient are resolved from the global catalog at runtime (policy is the
// single source); the client only carries the coefficient.
//
// Coefficient semantics (pointer to distinguish "unset" from 0):
//   - nil        → default 1（中性，不缩放）
//   - 0          → 该客户端在此组的额度不限（任一系数为 0 = 不限制）
//   - >0         → 有效额度乘数：limit = 组基数 × 职级系数 × 个人系数
type QuotaGroup struct {
	Name        string   `json:"name"`
	Coefficient *float64 `json:"coefficient,omitempty"`
}

// PlanCoefficient returns the effective personal coefficient for a plan entry:
// nil defaults to 1; explicit values (including 0 = unlimited) pass through.
func (g QuotaGroup) PlanCoefficient() float64 {
	if g.Coefficient == nil {
		return 1
	}
	return *g.Coefficient
}

// JobLevel 是结构化的职级记录：个人维度额度的兜底系数来源（额度体系 v2）。
// 三个系数按维度独立生效：有效额度(dim) = 组基数(dim) × 职级系数(dim) ×
// 个人系数。指针语义同 QuotaGroup.Coefficient：nil→1，0→不限，>0→乘数。
// 客户端未绑定职级（或绑定的职级不存在）= 未配置额度 = 拒绝请求。
type JobLevel struct {
	Name               string   `json:"name"`
	CoefficientDaily   *float64 `json:"coefficient_daily,omitempty"`
	CoefficientWeekly  *float64 `json:"coefficient_weekly,omitempty"`
	CoefficientMonthly *float64 `json:"coefficient_monthly,omitempty"`
}

// CoefficientFor returns the job-level coefficient for a quota dimension
// ("daily"|"weekly"|"monthly"): nil defaults to 1, explicit values pass through.
func (j JobLevel) CoefficientFor(dimension string) float64 {
	var ptr *float64
	switch dimension {
	case QuotaDimensionDaily:
		ptr = j.CoefficientDaily
	case QuotaDimensionWeekly:
		ptr = j.CoefficientWeekly
	case QuotaDimensionMonthly:
		ptr = j.CoefficientMonthly
	}
	if ptr == nil {
		return 1
	}
	return *ptr
}

type QuotaGroupBinding struct {
	Target string `json:"target"`
	Model  string `json:"model,omitempty"`
}

// ModelGroup is the global model-to-group mapping used by client quotas.
type ModelGroup struct {
	Name            string              `json:"name"`
	CostCoefficient float64             `json:"cost_coefficient"`
	DailyUSD        float64             `json:"daily_usd,omitempty"`
	WeeklyUSD       float64             `json:"weekly_usd,omitempty"`
	MonthlyUSD      float64             `json:"monthly_usd,omitempty"`
	Bindings        []QuotaGroupBinding `json:"bindings"`
}

// LoggingConfig contains logging level and file paths.
type LoggingConfig struct {
	Level     string `json:"level"`
	AccessLog string `json:"access_log"`
	ErrorLog  string `json:"error_log"`
}

// Manager caches configuration content loaded from disk.
type Manager struct {
	path string

	mu    sync.RWMutex
	cache *Config
}

// NewManager creates a Manager bound to a config file path.
func NewManager(path string) *Manager {
	return &Manager{path: path}
}

// Path returns the configured config file path.
func (m *Manager) Path() string {
	return m.path
}

// Current returns the cached configuration. If not yet loaded it attempts to load it.
func (m *Manager) Current() (*Config, error) {
	m.mu.RLock()
	if m.cache != nil {
		cfg := m.cache.Clone()
		m.mu.RUnlock()
		return cfg, nil
	}
	m.mu.RUnlock()

	return m.Reload()
}

// Reload forces reading the configuration file from disk.
func (m *Manager) Reload() (*Config, error) {
	cfg, err := Load(m.path)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	m.cache = cfg
	m.mu.Unlock()
	return cfg.Clone(), nil
}

// LoadOrInit loads the config file, creating a default one when missing.
// The second return value is true when a new default config was generated.
func (m *Manager) LoadOrInit() (*Config, bool, error) {
	cfg, created, err := LoadOrCreate(m.path)
	if err != nil {
		return nil, false, err
	}

	m.mu.Lock()
	m.cache = cfg
	m.mu.Unlock()
	return cfg.Clone(), created, nil
}

// Replace updates the cached configuration without reading from disk.
func (m *Manager) Replace(cfg *Config) {
	if m == nil {
		return
	}

	m.mu.Lock()
	if cfg == nil {
		m.cache = nil
	} else {
		m.cache = cfg.Clone()
	}
	m.mu.Unlock()
}

// DefaultConfig returns a minimal valid configuration suitable for first-time
// startup.  The system can boot with zero upstream targets; users add targets
// later via the admin UI.
//
// Paths use the container-standard absolute directories that match the
// Dockerfile VOLUME declarations so that the auto-generated config works
// out of the box in Docker / QNAP environments.
func DefaultConfig() *Config {
	return &Config{
		Server: ServerConfig{
			Bind:                  "0.0.0.0:8080",
			RequestTimeoutSeconds: 1800,
		},
		Targets: []Target{},
		DataStore: DataStore{
			DBPath: "/var/lib/llms-proxy/llms-proxy.db",
		},
		AdminSession: AdminSessionConfig{
			CookieName:        "llms_proxy_admin_session",
			Secret:            "change-me-on-first-login",
			TTLSeconds:        86400,
			SlidingExpiration: true,
			SecureCookie:      false,
		},
		Logging: LoggingConfig{
			Level:     "info",
			AccessLog: "/var/log/llms-proxy/access.log",
			ErrorLog:  "/var/log/llms-proxy/error.log",
		},
	}
}

// Load reads and validates the config file from path.
func Load(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("config: open %s: %w", path, err)
	}
	defer f.Close()

	content, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}

	cfg, err := parse(content)
	if err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
	}

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("config: validate %s: %w", path, err)
	}

	resolveDataFilePaths(cfg, filepath.Dir(path))

	return cfg, nil
}

// LoadOrCreate tries to load an existing config file.  When the file does not
// exist it writes a default configuration to disk and returns it.  The second
// return value is true when a new file was created.
func LoadOrCreate(path string) (*Config, bool, error) {
	cfg, err := Load(path)
	if err == nil {
		return cfg, false, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, false, err
	}

	// File does not exist — generate default config and persist it.
	cfg = DefaultConfig()
	data, marshalErr := json.MarshalIndent(cfg, "", "  ")
	if marshalErr != nil {
		return nil, false, fmt.Errorf("config: marshal default: %w", marshalErr)
	}
	data = append(data, '\n')

	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		if mkErr := os.MkdirAll(dir, 0o755); mkErr != nil {
			return nil, false, fmt.Errorf("config: create dir %s: %w", dir, mkErr)
		}
	}
	if writeErr := os.WriteFile(path, data, 0o644); writeErr != nil {
		return nil, false, fmt.Errorf("config: write default %s: %w", path, writeErr)
	}

	resolveDataFilePaths(cfg, filepath.Dir(path))
	return cfg, true, nil
}

// RemoveTargetsFromFile removes legacy target definitions from a config file
// after they have been migrated into the datastore.
func RemoveTargetsFromFile(path string) error {
	content, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("config: read %s: %w", path, err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(content, &raw); err != nil {
		return fmt.Errorf("config: parse %s: %w", path, err)
	}
	if _, ok := raw["targets"]; !ok {
		return nil
	}
	delete(raw, "targets")
	payload, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return fmt.Errorf("config: marshal %s: %w", path, err)
	}
	payload = append(payload, '\n')
	tmp := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".tmp")
	if err := os.WriteFile(tmp, payload, 0o644); err != nil {
		return fmt.Errorf("config: write temp %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("config: replace %s: %w", path, err)
	}
	return nil
}

// RemovePoolsFromFile removes legacy deployment pool definitions from a config
// file after they have been migrated into the datastore.
func RemovePoolsFromFile(path string) error {
	content, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("config: read %s: %w", path, err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(content, &raw); err != nil {
		return fmt.Errorf("config: parse %s: %w", path, err)
	}
	if _, ok := raw["pools"]; !ok {
		return nil
	}
	delete(raw, "pools")
	payload, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return fmt.Errorf("config: marshal %s: %w", path, err)
	}
	payload = append(payload, '\n')
	tmp := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".tmp")
	if err := os.WriteFile(tmp, payload, 0o644); err != nil {
		return fmt.Errorf("config: write temp %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("config: replace %s: %w", path, err)
	}
	return nil
}

func parse(data []byte) (*Config, error) {
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	// Preserve presence and the original spelling before Validate normalizes
	// values. A plain string cannot distinguish missing from explicit empty.
	var input struct {
		Targets []struct {
			UsageAdapter json.RawMessage `json:"usage_adapter"`
		} `json:"targets"`
	}
	if err := json.Unmarshal(data, &input); err != nil {
		return nil, err
	}
	for i, target := range input.Targets {
		value := ""
		if target.UsageAdapter != nil {
			if err := json.Unmarshal(target.UsageAdapter, &value); err != nil {
				return nil, err
			}
		}
		cfg.Targets[i].usageAdapterInput = &usageAdapterInput{value: value, present: target.UsageAdapter != nil}
	}

	return &cfg, nil
}

// Validate performs semantic checks on the configuration.
func (c *Config) Validate() error {
	var problems []string

	if strings.TrimSpace(c.Server.Bind) == "" {
		problems = append(problems, "server.bind must not be empty")
	}

	if c.Server.RequestTimeoutSeconds <= 0 {
		problems = append(problems, "server.request_timeout_seconds must be greater than zero")
	}

	for i, target := range c.Targets {
		target = NormalizeTargetForCompatibility(target)
		target.UsageAdapter = NormalizeUsageAdapter(target.UsageAdapter)
		c.Targets[i] = target
		prefix := fmt.Sprintf("targets[%d]", i)

		// Normalise and validate endpoint_type.
		epType := NormalizeEndpointType(target.EndpointType)
		if !IsValidEndpointType(epType) {
			problems = append(problems, fmt.Sprintf("%s endpoint_type %q is not valid; must be one of: %s",
				prefix, target.EndpointType, strings.Join(ValidEndpointTypes, ", ")))
		}

		if strings.TrimSpace(target.Name) == "" {
			problems = append(problems, prefix+" name must not be empty")
		}
		if strings.TrimSpace(target.Endpoint) == "" {
			problems = append(problems, prefix+" endpoint must not be empty")
		}
		// Legacy file targets can contain empty/auto values for the database
		// import path only. Stores and admin writes require explicit selections.
		if target.UsageAdapter != "" && target.UsageAdapter != UsageAdapterAuto && !IsValidUsageAdapter(target.UsageAdapter) {
			problems = append(problems, fmt.Sprintf("%s usage_adapter %q is not valid; must be one of: %s", prefix, target.UsageAdapter, strings.Join(EnumValues(AllUsageAdapterOptions()), ", ")))
		}

		if err := ValidateModelPrefix(target.ModelPrefix); err != nil {
			problems = append(problems, prefix+" "+err.Error())
		}

		// Unknown auth_mode / image_operation are tolerated at runtime (default
		// auth, no image routing); rejecting them here would block startup.

		hasAnyKey := strings.TrimSpace(target.APIKey) != ""
		if !hasAnyKey {
			for _, k := range target.APIKeys {
				if strings.TrimSpace(k) != "" {
					hasAnyKey = true
					break
				}
			}
		}
		if !hasAnyKey && !target.AllowBearer {
			problems = append(problems, prefix+" api_key must not be empty when allow_bearer_passthrough is false")
		}
		upstreamSet := make(map[string]bool)
		fallbackSet := make(map[string]bool)
		for j, mm := range target.ModelMappings {
			up := strings.TrimSpace(mm.Upstream)
			if up == "" {
				problems = append(problems, fmt.Sprintf("%s model_mappings[%d].upstream must not be empty", prefix, j))
				continue
			}
			upLow := strings.ToLower(up)
			if upstreamSet[upLow] {
				problems = append(problems, fmt.Sprintf("%s model_mappings[%d].upstream %q is duplicate (case-insensitive)", prefix, j, up))
			}
			upstreamSet[upLow] = true
			for _, alias := range SplitFallbackAliases(mm.Fallback) {
				aliasLow := strings.ToLower(alias)
				if upstreamSet[aliasLow] {
					problems = append(problems, fmt.Sprintf("%s model_mappings[%d].fallback %q conflicts with an upstream name", prefix, j, alias))
				}
				if fallbackSet[aliasLow] {
					problems = append(problems, fmt.Sprintf("%s model_mappings[%d].fallback %q is duplicate (case-insensitive)", prefix, j, alias))
				}
				fallbackSet[aliasLow] = true
			}
		}
	}

	if strings.TrimSpace(c.DataStore.DBPath) == "" {
		problems = append(problems, "data_store.db_path must not be empty")
	}

	// 部署池校验：name 非空 + 小写去重（成员引用存在性由运行时 store 校验）。
	poolNames := make(map[string]struct{}, len(c.Pools))
	for i, pool := range c.Pools {
		if strings.TrimSpace(pool.Name) == "" {
			problems = append(problems, fmt.Sprintf("pools[%d].name must not be empty", i))
			continue
		}
		key := strings.ToLower(strings.TrimSpace(pool.Name))
		if _, dup := poolNames[key]; dup {
			problems = append(problems, fmt.Sprintf("pools[%d].name %q is duplicate (case-insensitive)", i, pool.Name))
		}
		poolNames[key] = struct{}{}
	}

	if strings.TrimSpace(c.AdminSession.CookieName) == "" {
		problems = append(problems, "admin_session.cookie_name must not be empty")
	}
	if strings.TrimSpace(c.AdminSession.Secret) == "" {
		problems = append(problems, "admin_session.secret must not be empty")
	}
	if c.AdminSession.TTLSeconds <= 0 {
		problems = append(problems, "admin_session.ttl_seconds must be greater than zero")
	}

	if strings.TrimSpace(c.Logging.Level) == "" {
		problems = append(problems, "logging.level must not be empty")
	}
	if strings.TrimSpace(c.Logging.AccessLog) == "" {
		problems = append(problems, "logging.access_log must not be empty")
	}
	if strings.TrimSpace(c.Logging.ErrorLog) == "" {
		problems = append(problems, "logging.error_log must not be empty")
	}

	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

// Clone returns a deep copy of the Config to avoid accidental mutation.
func (c *Config) Clone() *Config {
	if c == nil {
		return nil
	}

	clone := *c
	if len(c.Targets) > 0 {
		clone.Targets = make([]Target, len(c.Targets))
		for i := range c.Targets {
			clone.Targets[i] = c.Targets[i]
			if len(c.Targets[i].APIKeys) > 0 {
				clone.Targets[i].APIKeys = append([]string(nil), c.Targets[i].APIKeys...)
			}
			if len(c.Targets[i].ModelMappings) > 0 {
				clone.Targets[i].ModelMappings = append([]ModelMapping(nil), c.Targets[i].ModelMappings...)
			}
			// Deep copy map fields to prevent shared mutation.
			if c.Targets[i].CustomHeaders != nil {
				clone.Targets[i].CustomHeaders = make(map[string]string, len(c.Targets[i].CustomHeaders))
				for k, v := range c.Targets[i].CustomHeaders {
					clone.Targets[i].CustomHeaders[k] = v
				}
			}
			if c.Targets[i].CustomBody != nil {
				clone.Targets[i].CustomBody = make(map[string]any, len(c.Targets[i].CustomBody))
				for k, v := range c.Targets[i].CustomBody {
					if v != nil {
						clone.Targets[i].CustomBody[k] = cloneJSONValue(reflect.ValueOf(v)).Interface()
					} else {
						clone.Targets[i].CustomBody[k] = nil
					}
				}
			}
		}
	}

	// DeploymentPool 元素无引用字段，slice 拷贝即深拷贝。
	if len(c.Pools) > 0 {
		clone.Pools = append([]DeploymentPool(nil), c.Pools...)
	}

	return &clone
}

func resolveDataFilePaths(cfg *Config, baseDir string) {
	if cfg == nil {
		return
	}
	cfg.DataStore.DBPath = resolvePath(baseDir, cfg.DataStore.DBPath)
	cfg.DataFiles.ClientsFile = resolvePath(baseDir, cfg.DataFiles.ClientsFile)
	cfg.DataFiles.ModelCostsFile = resolvePath(baseDir, cfg.DataFiles.ModelCostsFile)
	cfg.DataFiles.UsageEventsFile = resolvePath(baseDir, cfg.DataFiles.UsageEventsFile)
	cfg.DataFiles.AdminUsersFile = resolvePath(baseDir, cfg.DataFiles.AdminUsersFile)
	cfg.DataFiles.AdminAuditFile = resolvePath(baseDir, cfg.DataFiles.AdminAuditFile)
	cfg.Logging.AccessLog = resolvePath(baseDir, cfg.Logging.AccessLog)
	cfg.Logging.ErrorLog = resolvePath(baseDir, cfg.Logging.ErrorLog)
}

func resolvePath(baseDir, path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	if strings.TrimSpace(baseDir) == "" {
		return filepath.Clean(path)
	}
	return filepath.Clean(filepath.Join(baseDir, path))
}

// cloneJSONValue preserves concrete JSON-compatible map/slice types and numeric
// values while isolating nested request options in configuration snapshots.
func cloneJSONValue(value reflect.Value) reflect.Value {
	switch value.Kind() {
	case reflect.Interface:
		if value.IsNil() {
			return value
		}
		out := reflect.New(value.Type()).Elem()
		out.Set(cloneJSONValue(value.Elem()))
		return out
	case reflect.Map:
		if value.IsNil() {
			return value
		}
		out := reflect.MakeMapWithSize(value.Type(), value.Len())
		iter := value.MapRange()
		for iter.Next() {
			out.SetMapIndex(iter.Key(), cloneJSONValue(iter.Value()))
		}
		return out
	case reflect.Slice:
		if value.IsNil() {
			return value
		}
		out := reflect.MakeSlice(value.Type(), value.Len(), value.Len())
		for i := 0; i < value.Len(); i++ {
			out.Index(i).Set(cloneJSONValue(value.Index(i)))
		}
		return out
	default:
		return value
	}
}

// ValidateModelPrefix preserves operator spelling and punctuation. URL builders
// encode path punctuation; control characters are never valid model identifiers.
func ValidateModelPrefix(prefix string) error {
	for _, r := range prefix {
		if unicode.IsControl(r) {
			return errors.New("model_prefix must not contain control characters")
		}
	}
	return nil
}
