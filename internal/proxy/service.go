// service.go — 代理服务核心编排：生命周期管理与请求主循环。
package proxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/errorlog"
	appmiddleware "github.com/LeXwDeX/LLM-Proxy/internal/middleware"
	"github.com/LeXwDeX/LLM-Proxy/internal/quota"
	"github.com/LeXwDeX/LLM-Proxy/internal/requeststats"
	"github.com/LeXwDeX/LLM-Proxy/internal/tracestore"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"

	"github.com/LeXwDeX/LLM-Proxy/internal/admission"
)

const (
	headerProxyTarget = "X-Proxy-Target"
	headerProxyPool   = "X-Proxy-Pool"
)

// maxRequestBodyBytes 限制单次请求体大小，防止恶意超大请求引发 OOM。
// 100MB 覆盖多模态图片输入（base64 编码后）等合法大请求场景。
const maxRequestBodyBytes = 100 * 1024 * 1024

// Service forwards authenticated requests to upstream targets.
type Service struct {
	logger                 *slog.Logger
	httpClient             *http.Client
	automaticGzipTransport *http.Transport
	responsesWS            *responsesWSPool
	requestTimeout         atomic.Int64
	quietPeriod            time.Duration

	usageMu                    sync.RWMutex
	usageRecorder              usage.Recorder
	requestStatsRecorder       requeststats.Recorder
	requestStatsRecordFailures atomic.Int64
	requestStatsLog            failureLogLimiter

	mu                      sync.RWMutex
	targetsByName           map[string]*targetState
	targetOrder             []*targetState
	targetRevision          uint64
	targetRevisionExhausted bool

	affinity *affinityMap

	// Trace store for DEBUG mode (nil = disabled)
	traceStore *tracestore.Store

	providerRegistry *ProviderRegistry // endpoint_type → ProviderProfile 映射

	// quotaManager 配额管理器（nil = 未启用配额准入）
	// 使用 atomic.Pointer 支持安全的并发读写（为未来热切换预留）。
	quotaManager atomic.Pointer[quota.Manager]

	// admissionManager 上游容量准入控制器（nil = 未启用）
	// WP-2: 按 target 实施令牌桶 + AIMD 自适应率 + 有界队列，防止压垮上游频率限制。
	admissionManager atomic.Pointer[admission.Manager]

	metrics   requestMetrics
	startTime time.Time
	activity  *activityTracker
	rrCounter atomic.Uint64

	websocketMu       sync.Mutex
	websocketSessions map[*responsesWebsocketLifecycle]struct{}
	websocketClients  map[string]int // native sessions per client credential
	websocketDrain    chan struct{}  // closed when sessions should end after their turn
	websocketsClosing bool
	// Native session limits, read once at construction.
	websocketIdleTimeout  time.Duration
	websocketPingInterval time.Duration
	websocketPerClient    int
}

// Target represents an upstream endpoint with runtime metadata.
type Target struct {
	Name           string
	EndpointType   string // azure_openai | openai | claude | gemini | openai_image | dual_protocol
	UsageAdapter   string // vendor patch policy; protocol remains path-derived
	Endpoint       *url.URL
	APIKey         string
	APIKeys        []string // 合并后的有序 key 池 [api_key, api_keys...]
	KeyResetTime   string   // 额度重置时间点（CST）
	Paused         bool
	Pool           string // 所属部署池（小写；""=未分组）
	MaxConcurrency int    // S1 惰性（S2 激活），0=未设置
	AllowBearer    bool
	AuthMode       string
	ImageOperation string
	ModelMappings  []config.ModelMapping
	ModelPrefix    string // Added only when constructing the outbound model; excluded from ACL and pricing.
	// dual_protocol 专用字段
	OpenAIPrefix      string            // OpenAI 路径前缀
	AnthropicPrefix   string            // Anthropic 路径前缀
	SupportsResponses bool              // 是否支持 /v1/responses
	PreferWebSocket   bool              // openai synchronous Responses transport preference
	CustomHeaders     map[string]string // 自定义 HTTP 请求头
	CustomBody        map[string]any    // 自定义请求体 JSON 顶层字段
	allowedModelIdx   map[string]string // lowercase(upstream OR fallback) → upstream (original case)

	RequestStreamUsageIfMissing bool // Opt-in for actual OpenAI Chat stream requests.
}

// SupportsPath 检查 target 是否支持给定的请求路径。
// 综合 endpoint_type 级别的路径能力检查和 per-target 的配置（如 SupportsResponses）。
func (t *Target) SupportsPath(path string) bool {
	if !PathSupportedByEndpointType(t.EndpointType, path) {
		return false
	}
	if t.EndpointType == config.EndpointTypeOpenAIImage {
		return openAIImageOperationSupportsPath(t.ImageOperation, path)
	}
	if t.EndpointType == config.EndpointTypeDualProtocol && requestSchemaForPath(path) == config.RequestSchemaOpenAIResponses && !t.SupportsResponses {
		return false
	}
	return true
}

type requestMetrics struct {
	totalRequests            atomic.Int64
	totalSuccess             atomic.Int64
	totalFailures            atomic.Int64
	totalUsageRecordFailures atomic.Int64
	totalKeyRetries          atomic.Int64 // key pool 内换 key 重试
	totalTargetRetries       atomic.Int64 // target 级 failover 重试
	activeRequests           atomic.Int64
	totalClientCanceled      atomic.Int64 // 客户端在响应返回前断开（499），不计入 failures
	// Zero-token events recorded because the client left before the
	// upstream's official final usage was received (no replay).
	totalUsageClientGoneNoUsage atomic.Int64
	// Zero-token events recorded because the upstream response ended for any
	// other reason before its official final usage (no replay).
	totalUsageUpstreamInterruptedNoUsage atomic.Int64
	// Requests (HTTP or native WS create) whose duplicate or case-variant
	// protected members were removed at entry.
	totalRequestMembersNormalized atomic.Int64
}

// ServiceMetrics captures aggregate request statistics.
type ServiceMetrics struct {
	TotalRequests            int64
	TotalSuccess             int64
	TotalFailures            int64
	TotalUsageRecordFailures int64 // usage could not be persisted to the primary store or recovery journal
	// TotalUsageClientGoneNoUsage counts zero-token events written because the
	// client disconnected before the official final usage arrived.
	TotalUsageClientGoneNoUsage int64
	// TotalUsageUpstreamInterruptedNoUsage counts zero-token events written
	// because the upstream response ended without official final usage for a
	// reason other than the client leaving.
	TotalUsageUpstreamInterruptedNoUsage int64
	// TotalRequestMembersNormalized counts requests whose duplicate or
	// case-variant protected members were removed at entry.
	TotalRequestMembersNormalized int64
	TotalRetries                  int64 // 总重试 = KeyRetries + TargetRetries（向后兼容）
	TotalKeyRetries               int64 // key pool 内换 key 重试次数
	TotalTargetRetries            int64 // target 级 failover 重试次数
	TotalClientCanceled           int64 // 客户端在响应返回前断开（499），非代理失败
	ActiveRequests                int64
	StartTime                     time.Time
	// ResponsesWS covers the upstream Responses WebSocket pool and handshakes.
	ResponsesWS ResponsesWSMetrics
}

// TargetStatus summarizes the health of a configured target.
type TargetStatus struct {
	Name                 string
	EndpointType         string
	Endpoint             string
	Paused               bool
	Muted                bool
	MutedUntil           time.Time
	LastSuccess          time.Time
	LastFailure          time.Time
	ConsecutiveFailures  int
	TotalSuccessRequests int64
	TotalFailedRequests  int64
	// Key pool status (only populated if target has multiple keys)
	TotalKeys                 int
	ActiveKeys                int
	ExhaustedSubscriptionKeys int // quota_exceeded_subscription (自动恢复)
	ExhaustedAPIKeys          int // quota_exceeded_api (人工恢复)
	RateLimitedKeys           int
	BlockedKeys               int
	OtherExhaustedKeys        int // 其他失效原因 (invalid_token/billing_error/account_disabled/旧版 quota_exceeded)，5 分钟冷却自动恢复
	// S2 容量感知调度（议题 #15）：节点级在途与容量。
	// Capacity=0 表示未配置上限（不限流）；Utilization 仅在 Capacity>0 时有意义。
	Capacity    int
	InFlight    int64
	Utilization float64
	// S5 调度可观测（议题 #18）：派生相位（ready/muted/half-open/offline）与
	// 节点级亲和命中、熔断次数（进程生命周期内累计）。
	Phase        string
	AffinityHits int64
	BreakerTrips int64
	Window15m    RuntimeWindow
}

// Wire transport connection settings.
const (
	// upstreamDialTimeout bounds the TCP dial. A dial failure provably sent
	// nothing, so it fails over quickly (isPreSendTransportError).
	upstreamDialTimeout   = 8 * time.Second
	upstreamDialKeepAlive = 30 * time.Second
	// tlsSessionCacheSize keeps TLS sessions for resumption across upstreams.
	tlsSessionCacheSize = 256
	// HTTP/2 dead-connection detection: ping an idle-read connection after
	// 30s and close it if no reply arrives within 15s. No retry is added.
	http2SendPingTimeout = 30 * time.Second
	http2PingTimeout     = 15 * time.Second
)

// NewService creates a proxy service from configuration.
func NewService(cfg *config.Config, logger *slog.Logger) (*Service, error) {
	if cfg == nil {
		return nil, errors.New("proxy: config must not be nil")
	}

	wireTransport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DisableCompression:    true, // Validate all coding fields before lazy decoding.
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   50,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		DialContext:           (&net.Dialer{Timeout: upstreamDialTimeout, KeepAlive: upstreamDialKeepAlive}).DialContext,
		// Session resumption skips full handshakes on new connections,
		// including upstream WebSocket dials that share this transport.
		TLSClientConfig: &tls.Config{ClientSessionCache: tls.NewLRUClientSessionCache(tlsSessionCacheSize)},
		// Required: a custom TLSClientConfig or DialContext otherwise turns
		// HTTP/2 off silently.
		ForceAttemptHTTP2: true,
		HTTP2:             &http.HTTP2Config{SendPingTimeout: http2SendPingTimeout, PingTimeout: http2PingTimeout},
	}
	service := &Service{
		logger:                 logger,
		automaticGzipTransport: wireTransport,
		httpClient: &http.Client{
			Transport: wireTransport,
		},
		quietPeriod: defaultQuietPeriod(),
		responsesWS: newResponsesWSPool(),

		websocketIdleTimeout:  responsesWebsocketIdleTimeout,
		websocketPingInterval: responsesWebsocketPingInterval,
		websocketPerClient:    responsesWebsocketPerClient,
		targetsByName:         make(map[string]*targetState),
		affinity:              newAffinityMap(),
		startTime:             time.Now(),
	}
	service.responsesWS.automaticGzipTransport = wireTransport
	service.setRequestTimeout(time.Duration(cfg.Server.RequestTimeoutSeconds) * time.Second)
	service.activity = newActivityTracker(service.startTime)
	// S5（议题 #18）：静默基线冷却可通过 quiet_period_seconds 调整（0/负值保持 60s 默认），
	// 供运维调优与端到端集成验收压缩熔断恢复窗口。
	if v := cfg.Server.QuietPeriodSeconds; v > 0 {
		service.quietPeriod = time.Duration(v) * time.Second
	}
	registry, err := NewDefaultProviderRegistry()
	if err != nil {
		return nil, err
	}
	service.providerRegistry = registry

	// Initialize trace store (DEBUG mode only)
	traceCfg := tracestore.Config{
		Enabled:        cfg.TraceStore.Enabled,
		RingBufferSize: cfg.TraceStore.RingBufferSize,
		MaxBodySize:    cfg.TraceStore.MaxBodySize,
		DiskPath:       cfg.TraceStore.DiskPath,
		DiskMaxSizeMB:  cfg.TraceStore.DiskMaxSizeMB,
		DiskMaxBackups: cfg.TraceStore.DiskMaxBackups,
		DiskTTLHours:   cfg.TraceStore.DiskTTLHours,
		ChannelBuffer:  cfg.TraceStore.ChannelBuffer,
	}
	// Apply defaults for zero values
	if traceCfg.RingBufferSize == 0 {
		traceCfg.RingBufferSize = 1000
	}
	if traceCfg.MaxBodySize == 0 {
		traceCfg.MaxBodySize = 512 * 1024 // 512KB
	}
	if traceCfg.DiskPath == "" {
		traceCfg.DiskPath = "/var/lib/llms-proxy/trace.log"
	}
	if traceCfg.DiskMaxSizeMB == 0 {
		traceCfg.DiskMaxSizeMB = 500 // 500MB per file
	}
	if traceCfg.DiskMaxBackups == 0 {
		traceCfg.DiskMaxBackups = 10 // 10 files = 5GB total
	}
	if traceCfg.DiskTTLHours == 0 {
		traceCfg.DiskTTLHours = 120 // 5 days
	}
	if traceCfg.ChannelBuffer == 0 {
		traceCfg.ChannelBuffer = 500
	}
	service.traceStore = tracestore.New(traceCfg, logger)

	if err := service.ApplyConfig(cfg); err != nil {
		return nil, err
	}

	return service, nil
}

// UpdateTargets refreshes the known targets from configuration.
func (s *Service) UpdateTargets(targets []config.Target) error {
	parsed, order, err := buildTargetStates(targets, s.logger)
	if err != nil {
		return err
	}

	s.mu.Lock()
	s.replaceTargetsLocked(parsed, order)
	s.mu.Unlock()

	return nil
}

func (s *Service) advanceTargetRevisionLocked() {
	if !s.targetRevisionExhausted {
		s.targetRevision++
		if s.targetRevision == 0 {
			// Zero stays unavailable after overflow so an old revision cannot recur.
			s.targetRevisionExhausted = true
		}
	}
}

// replaceTargetsLocked publishes new configuration while keeping the actual
// occupancy of each surviving target. The caller holds s.mu for writing.
func (s *Service) replaceTargetsLocked(parsed map[string]*targetState, order []*targetState) {
	s.advanceTargetRevisionLocked()
	previous := s.targetsByName
	if s.responsesWS != nil {
		s.responsesWS.invalidate(false)
	}
	for name, next := range parsed {
		if old := s.targetsByName[name]; old != nil {
			// Surviving generations share breaker state so in-flight outcomes land
			// on the state routing observes (#86). A new connection identity or a
			// resume starts a fresh state instead (#184); the old state stays with
			// the old generation and late results never reach the new one.
			if reason, fields, resumed := shouldForkTargetHealth(old.target, next.target); reason != "" {
				s.logTargetBreakerReset(next.target.Name, reason, fields, resumed, old.Phase(time.Now()), old.Stats().ConsecutiveFailure)
				next.targetHealthState = old.targetHealthState.forkForNewIdentity()
			} else {
				next.targetHealthState = old.targetHealthState
			}
			// Unchanged credentials share their live pool, including outcomes of
			// requests which started before this configuration was published.
			if sameKeyPoolConfig(old.keyPool, next.keyPool) {
				next.keyPool = old.keyPool
			} else if old.keyPool != nil && next.keyPool != nil {
				next.keyPool.shareRuntimeFrom(old.keyPool)
			}
			next.inFlight = old.inFlight
			next.runtimeWindow = old.runtimeWindow
		}
	}
	if manager := s.admissionManager.Load(); manager != nil {
		for name := range s.targetsByName {
			if _, survives := parsed[name]; !survives {
				manager.RemoveTarget(name)
			}
		}
	}
	s.targetsByName = parsed
	s.targetOrder = order
	// Spare upstream WebSocket lanes must not outlive their credential's
	// place in the key pool.
	for _, state := range parsed {
		if state.keyPool != nil && state.target.PreferWebSocket && s.responsesWS != nil {
			state.keyPool.setOnKeysUnavailable(s.responsesWS.retireSparesForCredentials)
		}
	}
	if manager := s.admissionManager.Load(); manager != nil {
		for name, state := range parsed {
			s.initAdmissionTargetLocked(manager, name, state, previous[name] == nil)
		}
	}
}

// ApplyConfig updates runtime settings based on a full configuration snapshot.
func (s *Service) ApplyConfig(cfg *config.Config) error {
	return s.applyConfig(cfg, 0)
}

// ErrTargetSnapshotChanged means the target configuration changed since it was read.
var ErrTargetSnapshotChanged = errors.New("target snapshot changed")

// ApplyConfigIfTargetRevision applies cfg only if the observed runtime revision remains current.
func (s *Service) ApplyConfigIfTargetRevision(cfg *config.Config, revision uint64) error {
	if revision == 0 {
		return ErrTargetSnapshotChanged
	}
	return s.applyConfig(cfg, revision)
}

func (s *Service) applyConfig(cfg *config.Config, revision uint64) error {
	if cfg == nil {
		return errors.New("proxy: config must not be nil")
	}

	// 物化池暂停：池 Paused 折叠进成员 target.Paused，复用全部现有
	// Paused 读取点（选择拒绝/粘连拒绝/状态展示），选择层零改动。
	effective := cfg.Targets
	if len(cfg.Pools) > 0 {
		effective = applyPoolPause(cfg.Targets, cfg.Pools)
	}

	parsed, order, err := buildTargetStates(effective, s.logger)
	if err != nil {
		return err
	}

	timeout := time.Duration(cfg.Server.RequestTimeoutSeconds) * time.Second

	s.mu.Lock()
	if revision != 0 && revision != s.targetRevision {
		s.mu.Unlock()
		return ErrTargetSnapshotChanged
	}
	s.replaceTargetsLocked(parsed, order)
	s.mu.Unlock()

	s.setRequestTimeout(timeout)
	return nil
}

// SetUsageRecorder configures usage recorder for best-effort tracking.
func (s *Service) SetUsageRecorder(recorder usage.Recorder) {
	s.usageMu.Lock()
	s.usageRecorder = recorder
	s.usageMu.Unlock()
}

// SetQuotaManager 注入 quota manager，启用准入阶段配额检查。
// m 为 nil 时跳过配额检查，请求不受影响。
func (s *Service) SetQuotaManager(m *quota.Manager) {
	s.quotaManager.Store(m)
}

// SetAdmissionManager 注入准入控制管理器（WP-2）。
// m 为 nil 时跳过准入检查，请求不受影响。
func (s *Service) SetAdmissionManager(m *admission.Manager) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.admissionManager.Store(m)
	if m == nil {
		return
	}
	// Targets published before the manager existed must still be registered
	// and receive key-pool notifications.
	for name, state := range s.targetsByName {
		s.initAdmissionTargetLocked(m, name, state, true)
	}
}

func (s *Service) currentUsageRecorder() usage.Recorder {
	s.usageMu.RLock()
	defer s.usageMu.RUnlock()
	return s.usageRecorder
}

// ServeHTTP implements http.Handler for forwarding requests.
func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// A socket is a transport session. Count business requests per create event.
	// Missing authentication still follows the ordinary unauthorized path below.
	if isResponsesWebsocketUpgrade(r) && r.URL.Path == responsesPath {
		if principal, ok := auth.PrincipalFromContext(r.Context()); ok && principal != nil {
			s.serveResponsesWebsocket(w, r)
			return
		}
	}
	completion := requeststats.Failure
	statsRecorder := s.currentRequestStatsRecorder()
	defer func() { s.recordRequestCompletion(statsRecorder, completion) }()
	s.metrics.totalRequests.Add(1)
	s.metrics.activeRequests.Add(1)
	requestOutcomeRecorded := false
	defer func() {
		if !requestOutcomeRecorded {
			s.metrics.totalFailures.Add(1)
		}
		s.metrics.activeRequests.Add(-1)
	}()

	principal, ok := auth.PrincipalFromContext(r.Context())
	if !ok || principal == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		requestOutcomeRecorded = true
		s.metrics.totalFailures.Add(1)
		return
	}
	pathKind := classifyClientPath(r.URL.Path)
	if pathKind == clientPathUnknown {
		writeRequestPathError(w, r.URL.Path)
		requestOutcomeRecorded = true
		s.metrics.totalFailures.Add(1)
		return
	}
	activityClient := s.activity.begin(principal.Name)
	defer func() { s.activity.end(activityClient) }()

	// Fast-path: if no upstream targets are configured, inform the caller.
	s.mu.RLock()
	hasTargets := len(s.targetOrder) > 0
	s.mu.RUnlock()
	if !hasTargets {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"no upstream targets configured, add targets via admin UI"}`))
		requestOutcomeRecorded = true
		s.metrics.totalFailures.Add(1)
		return
	}

	requested := strings.TrimSpace(r.Header.Get(headerProxyTarget))
	if requested == "" {
		requested = strings.TrimSpace(r.URL.Query().Get("target"))
	}
	requestedLower := strings.ToLower(requested)
	allowFallback := requestedLower == ""

	var allowed map[string]struct{}
	if !principal.AllowAll() {
		allowed = principal.AllowedTargetsSet()
	}

	bodyBytes, err := readAndBufferBody(r)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(r.Context().Err(), context.Canceled) {
			completion = requeststats.Canceled
		}
		s.logger.Error("failed to read request body",
			"request_id", appmiddleware.RequestIDFromContext(r.Context()),
			"error", errorlog.ErrorCategory(err),
		)
		http.Error(w, "failed to read request body", http.StatusBadRequest)
		requestOutcomeRecorded = true
		s.metrics.totalFailures.Add(1)
		return
	}

	// The proxy reads, authorizes and normalizes identity bodies only. An
	// encoded body would reach the upstream unread (no model, no
	// normalization), so it is refused, never forwarded.
	if len(bodyBytes) > 0 && hasRequestContentCoding(r.Header.Values("Content-Encoding")) {
		w.Header().Set("Accept-Encoding", "identity")
		writeClientRequestError(w, http.StatusUnsupportedMediaType, unsupportedContentEncodingCode,
			"request bodies with a Content-Encoding other than identity are not supported", r.URL.Path)
		s.metrics.totalFailures.Add(1)
		requestOutcomeRecorded = true
		return
	}

	// A multipart body a JSON parser, or another form parser, would read
	// differently from the proxy cannot be normalized into one reading. With
	// several Content-Type headers the proxy reads the first, copyHeaders
	// forwards all of them and the upstream may pick another.
	reason := ""
	if len(bodyBytes) > 0 && len(r.Header.Values("Content-Type")) > 1 {
		reason = "request has more than one Content-Type header"
	} else {
		reason = requestBodyAmbiguity(r.Header.Get("Content-Type"), bodyBytes)
	}
	if reason != "" {
		writeClientRequestError(w, http.StatusBadRequest, ambiguousRequestBodyCode, reason, r.URL.Path)
		s.metrics.totalFailures.Add(1)
		requestOutcomeRecorded = true
		return
	}
	// A JSON object declared as urlencoded (curl -d) is read as JSON; declare
	// it as JSON upstream too, so no upstream reads it as a form. r.Header is
	// what every attempt copies (failover included) and what traces record.
	if contentType, ok := urlEncodedJSONContentType(r.Header.Get("Content-Type"), bodyBytes); ok {
		r.Header.Set("Content-Type", contentType)
	}

	// Normalize protected members once, before model extraction, ACL and
	// every later body step: duplicates and case variants could otherwise let
	// an upstream read a different model, tier or budget than the proxy
	// authorized and billed. Native WS create turns arrive here too.
	normalized, membersNormalized := s.normalizeRequestMembers(r, bodyBytes)
	if membersNormalized {
		bodyBytes = normalized
		r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		r.ContentLength = int64(len(bodyBytes))
	}

	if r.Method == http.MethodGet && isListEndpoint(r.URL.Path) {
		local := &localStatsWriter{ResponseWriter: w}
		s.maybeHandleLocalList(local, r)
		completion = responseCompletion(r, local.status, local.err, local.err != nil, false)
		requestOutcomeRecorded = true
		s.metrics.totalSuccess.Add(1)
		return
	}

	// Azure-sanitized body is computed lazily — only when the selected target is Azure.
	// This avoids a full JSON unmarshal on every request for non-Azure targets.
	var sanitizedBody []byte
	var strippedFields []string
	sanitizedComputed := false

	var attempted map[string]struct{}
	attempt := 0
	requestedModel := extractModel(r, bodyBytes)
	model := strings.ToLower(requestedModel)
	if model == "" && (modelRequiredForRequest(r, pathKind) || s.anyTargetRequiresModel()) {
		writeClientRequestError(w, http.StatusBadRequest, "model_required", "model is required for request path "+strconv.Quote(r.URL.Path), r.URL.Path)
		s.metrics.totalFailures.Add(1)
		requestOutcomeRecorded = true
		return
	}

	// Admission is checked after each target selection so a failover target
	// cannot bypass its own rate bucket.

	// lastForwardStatus 追踪最后一次 forwardRequest 的状态码，用于 failover 耗尽后
	// selectTarget 失败时传播正确的 HTTP 状态码（如全池限流耗尽 → 429 而非 503）。
	lastForwardStatus := 0
	// Retain the mapped status only for HTTP 200 ChatCompletionResp errors.
	lastMinimaxFailure := false
	// lastQuotaInfo 伴随 429 状态码：如果 lastForwardStatus=429 且 isQuota=true，
	// writeUpstreamLimitedResponse 使用 resetAt 生成带恢复时间的响应体。
	var lastQuotaInfo struct {
		isQuota bool
		resetAt time.Time
	}
	// reqTotalFails 是请求级别的全局失败计数（WP-3），用于防止无限重试：
	// 累计 4 次 HTTP 429 / GLM 1302 后直接返回 429 给客户端。
	reqTotalFails := 0

	// 模型别名改写会临时替换 r.URL.Path（buildURL 需要看到上游名）。改写以原始
	// path 为基准每轮迭代重算，避免 failover 时把上一 target 的上游模型名污染
	// 到下一 target 的请求；函数返回时统一复位。
	origURLPath, origURLRawPath := r.URL.Path, r.URL.RawPath
	defer func() { r.URL.Path, r.URL.RawPath = origURLPath, origURLRawPath }()
	// pendingNoUsage is the latest attempt that may have reached the upstream
	// but ended before response headers. One client request records at most
	// one usage event: a later response owns it; otherwise the final pending
	// attempt is recorded when the request ends (registered after the path
	// restore so it still sees the attempt's path).
	var pendingNoUsage *pendingNoUsage
	defer func() {
		if pendingNoUsage != nil {
			s.recordPendingNoUsage(r, pendingNoUsage)
		}
	}()
	sessionID := sessionAffinityID(r)
	// model is the normalized client request model, before any target rewrite.
	// Read the original binding once so forwarding-stage failover cannot hijack it.
	var sessionKey, sessionBoundTarget string
	if sessionID != "" && requestedLower == "" {
		sessionKey = sessionAffinityKey(principal.Name, sessionID, model)
		sessionBoundTarget, _ = s.affinity.Get(sessionKey, time.Now())
	}
	// Share the existing 499 completion accounting with the guard before the
	// next target selection. Preserve initial admission's existing response.
	// A parent's deadline remains a failed 504, not a canceled 499.
	finishContext := func(err error, targetName string) {
		status := classifyTransportError(err)
		if status == StatusClientClosedRequest {
			completion = requeststats.Canceled
			s.metrics.totalClientCanceled.Add(1)
			s.logger.Warn("client disconnected before response",
				"request_id", appmiddleware.RequestIDFromContext(r.Context()),
				"target", targetName, "attempt", attempt, "status", status)
			w.WriteHeader(status)
		} else {
			http.Error(w, http.StatusText(status), status)
			s.metrics.totalFailures.Add(1)
		}
		requestOutcomeRecorded = true
	}
	var selectedState *targetState
	var selectedLease *probeLease
	releaseSelected := func() {
		if selectedState != nil {
			selectedState.releaseProbe(selectedLease)
		}
		selectedState, selectedLease = nil, nil
	}
	defer releaseSelected()

	for {
		// A target change ends the previous selection; same-target key retries
		// stay inside this iteration and retain their probe ownership.
		releaseSelected()
		if err := r.Context().Err(); attempt > 0 && err != nil {
			finishContext(err, "")
			return
		}
		attempt++
		state, lease, selKind, err := s.selectTarget(principal, clientIP(r), sessionBoundTarget, requestedLower, allowed, attempted, model, origURLPath, time.Now())
		if err != nil {
			var selErr *selectionError
			if errors.As(err, &selErr) {
				// 标准 HTTP 状态码：如果上一轮 forward 是 429（限流/限额），传播 429 让客户端退避；
				// 否则用 selectTarget 的原始状态码（503 服务不可用 / 400 参数错误等）。
				if lastForwardStatus == http.StatusTooManyRequests {
					// 使用上一轮 forward 记录的 quota 信息（selectTarget 失败时已脱离 keypool 上下文）
					writeUpstreamLimitedResponse(w, lastQuotaInfo.isQuota, lastQuotaInfo.resetAt)
				} else if lastMinimaxFailure {
					http.Error(w, http.StatusText(lastForwardStatus), lastForwardStatus)
				} else {
					http.Error(w, selErr.Message, selErr.Status)
				}
				s.metrics.totalFailures.Add(1)
				requestOutcomeRecorded = true
				return
			}
			s.logger.Error("target selection failed",
				"request_id", appmiddleware.RequestIDFromContext(r.Context()),
				"error", errorlog.ErrorCategory(err),
			)
			http.Error(w, "failed to select target", http.StatusServiceUnavailable)
			s.metrics.totalFailures.Add(1)
			requestOutcomeRecorded = true
			return
		}
		selectedState, selectedLease = state, lease

		target := state.Target()
		if target == nil {
			http.Error(w, "target unavailable", http.StatusServiceUnavailable)
			s.metrics.totalFailures.Add(1)
			requestOutcomeRecorded = true
			return
		}

		upstreamModel, modelErr := s.ensureModelAllowed(target, model)
		if modelErr != nil {
			http.Error(w, modelErr.Error(), http.StatusBadRequest)
			s.metrics.totalFailures.Add(1)
			requestOutcomeRecorded = true
			return
		}
		// Quota admission uses the actual selected target and upstream model:
		// model-group quota is checked before the client's personal quota.
		if qm := s.quotaManager.Load(); qm != nil {
			if info, exceeded := qm.CheckTarget(principal.Name, target.Name, upstreamModel); exceeded {
				writeQuotaExceededResponse(w, info)
				requestOutcomeRecorded = true
				s.metrics.totalFailures.Add(1)
				return
			}
		}

		targetKey := strings.ToLower(target.Name)
		if attempted == nil {
			attempted = make(map[string]struct{})
		}
		attempted[targetKey] = struct{}{}

		// 每轮迭代基于【不变的】bodyBytes/origURLPath 计算转发体与 path：
		// 禁止就地修改共享的 bodyBytes / 累积循环内 defer，否则 failover 到下一
		// target 时会把上一 target 的上游模型名带进请求。模型名改写延后到
		// sanitize/multipart/custom-inject 之后对 forwardBody 局部应用（见下方）。
		r.URL.Path, r.URL.RawPath = origURLPath, origURLRawPath
		forwardBody := bodyBytes

		// 查找 provider profile（用于 body 预处理策略）。
		profile := s.providerRegistry.Lookup(target.EndpointType)

		// Body sanitize: 按 provider 的 BodyPolicy.SanitizeFunc 净化请求体。
		// 目前仅 Azure 使用：白名单过滤不兼容字段。
		if profile != nil && profile.Body.SanitizeFunc != nil {
			if !sanitizedComputed {
				sanitizedBody, strippedFields = sanitizeTargetBody(profile, r, bodyBytes)
				sanitizedComputed = true
				if len(strippedFields) > 0 {
					s.logger.Debug("stripped unsupported request fields",
						"request_id", appmiddleware.RequestIDFromContext(r.Context()),
						"path", r.URL.Path,
						"fields", strings.Join(strippedFields, ","),
					)
				}
			}
			forwardBody = sanitizedBody
		}

		// 对不支持 multipart 的 target，自动将 multipart/form-data 转换为 JSON。
		// 部分上游网关（如 aigateway）只接受 application/json，不支持 multipart/form-data。
		// PreserveMultipart 为 true 的 provider（Azure、网宿图像端点）原生支持 multipart，保留原样。
		var origContentType string
		preserveMultipart := profile != nil && profile.Body.PreserveMultipart
		if !preserveMultipart && needsMultipartConvert(r) {
			if converted, newCT, convErr := convertMultipartToJSON(r, bodyBytes); convErr == nil {
				forwardBody = converted
				origContentType = r.Header.Get("Content-Type")
				r.Header.Set("Content-Type", newCT)
			} else {
				s.logger.Warn("multipart→JSON conversion failed, forwarding original",
					"request_id", appmiddleware.RequestIDFromContext(r.Context()),
					"target", target.Name,
					"error", errorlog.ErrorCategory(convErr),
				)
			}
		}

		// Apply operator fields after sanitize, then rewrite from the original
		// client model for this target alone (including failover attempts).
		forwardBody = prepareTargetBody(target, r, forwardBody, requestedModel, upstreamModel)
		var supplementaryModels []string
		var supplementaryReport []memberNormalization
		forwardBody, supplementaryModels, supplementaryReport = prepareSupplementaryAdmission(r.URL.Path, forwardBody)
		if len(supplementaryReport) != 0 {
			if !membersNormalized {
				s.metrics.totalRequestMembersNormalized.Add(1)
				membersNormalized = true
			}
			s.logNormalizedMembers(r, supplementaryReport)
		}
		// Check the same target and final wire identities, before consuming rate
		// tokens or acquiring node/key occupancy. Failover assembles and checks anew.
		if qm := s.quotaManager.Load(); qm != nil {
			for _, supplementaryModel := range supplementaryModels {
				if info, exceeded := qm.CheckTarget(principal.Name, target.Name, supplementaryModel); exceeded {
					writeQuotaExceededResponse(w, info)
					requestOutcomeRecorded = true
					s.metrics.totalFailures.Add(1)
					return
				}
			}
		}

		if am := s.admissionManager.Load(); am != nil {
			if !am.TryAcquire(target.Name) {
				// Fast path failed → slow path: enqueue and wait.
				if err := am.Wait(target.Name, r.Context()); err != nil {
					if errors.Is(err, context.Canceled) {
						completion = requeststats.Canceled
					}
					if errors.Is(err, admission.ErrQueueFull) {
						s.logger.Warn("[admission] queue full", "target", target.Name)
					}
					writeUpstreamLimitedResponse(w, false, time.Time{})
					s.metrics.totalFailures.Add(1)
					requestOutcomeRecorded = true
					return
				}
			}
			// Token supply is time-based; ReleaseToken remains a compatibility no-op.
			defer am.ReleaseToken(target.Name)
		}

		startedAt := time.Now()
		var resp *http.Response
		var cancel context.CancelFunc
		var keyIndex int
		var fErr error
		var desperationProbe bool // 标记是否为绝境探测

		// key 池内重试策略（WP-1 + WP-3：429 给同一 key 多次机会后退化换 key，全局上限安全阀）：
		// - 429 / GLM 1302 限流：标记 key → per-key 连续失败计数 → RETRY<2 sleep 1s 重试；
		//   RETRY≥2 换 key；累计 4 次失败（reqTotalFails≥4）直接返回 429
		// - key 耗尽（quota_exceeded 等）：标记 key 为 exhausted，切换到下一个 active key
		// - 绝境探测：所有 key 都 exhausted 时，选冷却最短的 key 做探测，成功则恢复，失败则延长冷却
		// fix-C 收敛：同一请求内连续相同的账户级硬失败码。>=2（不同 key 返回相同码）是
		// 强"同账户失效"信号，停止逐 key 重试，透传真实上游响应。
		// 评审回归修复（LOW-8910）：仅按"相同 reason"收敛——keyPool 无 per-key 账户归属，
		// 不同 reason 可能来自多账户混合池的不同账户，不应收敛（会误 abandon 健康 key + 误 mute）。
		convergeCode := ""
		convergeStreak := 0
		accountLevelFailure := false // MED-3: 命中收敛后透传时按失败计（mute + 不计 success）
		// Only the parent stops the rejection retry policy. A per-attempt
		// deadline after response headers does not replace the HTTP rejection.
		parentEnded := func() bool {
			// A response already obtained from a generation must still pass
			// through writeResponse, the usage settlement owner. Cancellation
			// forbids another attempt; it must not discard known or incomplete
			// usage. Keep this exception here for both handoff guards.
			if fErr == nil && resp != nil {
				wsBody := responseWSBody(resp)
				if resp.StatusCode < 400 || (wsBody != nil && (wsBody.generationSent || wsBody.local)) {
					return false
				}
			}
			if err := r.Context().Err(); err != nil {
				if resp != nil {
					resp.Body.Close()
					resp = nil
				}
				deferCancel(cancel)
				cancel = nil
				ended := &forwardAttemptError{status: classifyTransportError(err), err: err}
				// Keep a sent attempt's pending usage settlement.
				if prior, ok := fErr.(*forwardAttemptError); ok && prior != nil {
					ended.noUsage = prior.noUsage
				}
				fErr = ended
				return true
			}
			return false
		}
		for {
			if parentEnded() {
				break
			}
			// Another attempt actually follows a sent, unsettled one: announce
			// it (log + counter, will_retry); only the final outcome records.
			s.announceNoUsageRetry(r, pendingNoUsage)
			resp, cancel, keyIndex, fErr = s.forwardRequest(r, state, forwardBody, upstreamModel, lease)
			if parentEnded() {
				break
			}
			if wsBody := responseWSBody(resp); fErr == nil && wsBody != nil && (wsBody.generationSent || wsBody.local) {
				// A generation may already have run, or the proxy answered locally:
				// never feed these into key-retry loops. A rejected upgrade sent
				// nothing, so it rotates keys like an ordinary HTTP response.
				break
			}
			if fErr != nil {
				// 智谱 GLM 限流（HTTP 200 + SSE rate_limit_error，code 1308/1310/1302）：
				// forwardRequest 已 markExhausted 并返回 errGLMRateLimited（skipMute=true）。
				// 此处直接切下一 key，不做 sleep 退避（窗口耗尽，退避无意义）。
				// 所有 key 都限流时不再透传——直接 break，fErr 经下方 retryable 分支上浮外层
				// target failover 到其他可用渠道（如网宿-Claude），避免卡死在本 target。
				//
				// inFlight 生命周期：每次 forwardRequest 独立 acquire + 独立释放。
				// GLM 命中路径由 forwardRequest 内的函数级 defer 兜底释放（见 forward.go
				// GLM 块注释）。此处 continue 重新 forwardRequest，新旧 key 的 inFlight
				// 互不影响，计数保持平衡。
				if (errors.Is(fErr, errGLMRateLimited) || errors.Is(fErr, errMinimaxInBandError)) && state.keyPool != nil && keyIndex >= 0 {
					lastForwardStatus = fErr.(*forwardAttemptError).status
					lastQuotaInfo.isQuota, lastQuotaInfo.resetAt = state.keyPool.quotaExhaustionStatus()

					// WP-3：全局上限检查（仅在 selectNextActiveKey 换 key 时计数，
					// unmarkExhaustedForRetry 重试属于"坚持同一个 key"不计入）
					if reqTotalFails >= 4 {
						if errors.Is(fErr, errMinimaxInBandError) {
							break
						}
						s.logger.Warn("[keypool] Global retry limit reached after structured key error",
							"request_id", appmiddleware.RequestIDFromContext(r.Context()),
							"target", target.Name,
							"total_attempts", attempt,
							"total_key_switches", reqTotalFails,
						)
						// WP-2 C1/N1 fix: all-keys-exhausted signal to AIMD controller.
						// Safety valve preempts the selectNextActiveKey "" path where OnLimit normally fires;
						// call it here so ≥5-key pools still trigger rate reduction.
						if am := s.admissionManager.Load(); am != nil {
							am.OnLimit(target.Name)
						}
						writeUpstreamLimitedResponse(w, lastQuotaInfo.isQuota, lastQuotaInfo.resetAt)
						s.metrics.totalFailures.Add(1)
						requestOutcomeRecorded = true
						return
					}

					// WP-3：per-key 连续失败计数
					keyFails := state.keyPool.incrementFails(keyIndex)
					// 读回 reason 区分瞬时/额度类，必须在下行 unmarkExhaustedForRetry 之前（unmark 后 exhausted
					// 变 false，读回空串）。
					// 注：极端并发下 reason 可能为 ""（key 恰好被其他请求的冷却恢复洗白）。此时 isTransient=false →
					// 走换 key 路径，这是安全默认（不坚持一个状态不明的 key），非 bug，勿"修"成坚持同 key。
					reason := state.keyPool.exhaustReasonAt(keyIndex)
					isTransient := errors.Is(fErr, errGLMRateLimited) && (reason == glmRateLimitQPS || reason == glmRateLimitOverload)

					retryClientName := ""
					if principal != nil {
						retryClientName = principal.Name
					}

					// WP-3：RETRY<2 解除 exhausted 状态 + sleep 1s 重试同一个 key —— 仅对瞬时类生效。
					// 额度类（1308/1310/unknown）1 秒后不可能恢复，不做无意义的同 key 重试，直接走换 key 路径。
					if isTransient && keyFails < 2 {
						s.logger.Info("[keypool] GLM transient key failed but RETRY<2, sleep and retry same key",
							"request_id", appmiddleware.RequestIDFromContext(r.Context()),
							"target", target.Name,
							"client", retryClientName,
							"key_index", keyIndex,
							"key_fails", keyFails,
							"reason", reason,
						)
						// 解除 exhausted 状态让 selectKeyForClient 能重新选中该 key（WP-3 坚持机制）
						waitForKeyRetry(r.Context())
						if parentEnded() {
							break
						}
						state.keyPool.unmarkExhaustedForRetry(keyIndex)
						s.metrics.totalKeyRetries.Add(1)
						continue
					}
					// 额度类 OR 瞬时类 RETRY≥2：换 key，全局计数+1（驱动 4 次全局安全阀）
					if parentEnded() {
						break
					}
					reqTotalFails++
					nextKey, nextIdx := state.keyPool.selectNextActiveKey(keyIndex)
					if nextKey == "" {
						// 所有 key 都限流/耗尽：不再重新转发透传——那会拿到 200 限流 SSE 走
						// writeResponse 成功路径，既不 mute 也不 failover，请求卡死在本 target。
						// 这里直接 break，fErr 仍是 errGLMRateLimited（retryable+skipMute），
						// 落到下方 retryable 分支：skipMute 跳过 handleForwardError（keypool 冷却自管，
						// 不误伤显式目标），allowFallback 时外层 selectTarget 把请求 failover 到其他可用渠道。
						s.logger.Warn("[keypool] Structured key error: all keys exhausted, escalating to target failover",
							"request_id", appmiddleware.RequestIDFromContext(r.Context()),
							"target", target.Name,
						)
						// WP-2 C1 fix: all keys exhausted is an upstream overload signal →
						// notify AIMD controller to decrease rate (OnLimit was previously dead code).
						if am := s.admissionManager.Load(); am != nil {
							am.OnLimit(target.Name)
						}
						break
					}
					if isTransient {
						s.logger.Info("[keypool] GLM transient key RETRY>=2, switching to next key",
							"request_id", appmiddleware.RequestIDFromContext(r.Context()),
							"target", target.Name,
							"client", retryClientName,
							"prev_key_index", keyIndex,
							"prev_key_fails", keyFails,
							"reason", reason,
							"next_key_index", nextIdx,
						)
					} else {
						s.logger.Info("[keypool] Structured key error, skip same-key retry, switching key",
							"request_id", appmiddleware.RequestIDFromContext(r.Context()),
							"target", target.Name,
							"client", retryClientName,
							"prev_key_index", keyIndex,
							"reason", reason,
							"next_key_index", nextIdx,
						)
					}
					if parentEnded() {
						break
					}
					s.metrics.totalKeyRetries.Add(1)
					continue
				}
				break
			}
			if resp == nil || resp.StatusCode < 400 {
				// 请求成功：如果是绝境探测，标记 key 为已恢复
				if desperationProbe && state.keyPool != nil && keyIndex >= 0 {
					state.keyPool.markRecovered(keyIndex)
				}
				break
			}
			// 429 限流：WP-3 — 标记 key，per-key 计数，RETRY<2 sleep 重试，RETRY≥2 换 key，全局上限 4 次
			if resp.StatusCode == 429 && state.keyPool != nil && keyIndex >= 0 {
				lastForwardStatus = http.StatusTooManyRequests // 标记限流，failover 耗尽时传播 429
				// Native rejections were classified once using their full bounded
				// envelope. A 4 KiB reread must not downgrade quota/vendor reasons.
				if responseWebsocketRejection(resp) == nil {
					body429, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
					// Only identity bytes can refine an HTTP 429 into vendor
					// quota/account feedback. Other encodings retain generic retry.
					if usesGLMErrorPatch(target) && responseIsIdentityEncoding(resp.Header) {
						state.keyPool.markRateLimitedWithBody(keyIndex, body429)
					} else {
						state.keyPool.markRateLimited(keyIndex)
					}
				}
				resp.Body.Close()
				resp = nil
				if cancel != nil {
					cancel()
					cancel = nil
				}
				if parentEnded() {
					break
				}

				// WP-3：全局上限检查（仅在 selectNextActiveKey 换 key 时计数，
				// unmarkExhaustedForRetry 重试属于"坚持同一个 key"不计入）
				if reqTotalFails >= 4 {
					lastQuotaInfo.isQuota, lastQuotaInfo.resetAt = state.keyPool.quotaExhaustionStatus()
					s.logger.Warn("[keypool] Global retry limit reached after key switch (HTTP 429), returning 429",
						"request_id", appmiddleware.RequestIDFromContext(r.Context()),
						"target", target.Name,
						"total_attempts", attempt,
						"total_key_switches", reqTotalFails,
					)
					// WP-2 C1/N1 fix: all-keys-exhausted signal to AIMD controller.
					// Safety valve preempts the selectNextActiveKey "" path where OnLimit normally fires;
					// call it here so ≥5-key pools still trigger rate reduction.
					if am := s.admissionManager.Load(); am != nil {
						am.OnLimit(target.Name)
					}
					writeUpstreamLimitedResponse(w, lastQuotaInfo.isQuota, lastQuotaInfo.resetAt)
					s.metrics.totalFailures.Add(1)
					requestOutcomeRecorded = true
					return
				}

				// WP-3：per-key 连续失败计数
				keyFails := state.keyPool.incrementFails(keyIndex)
				// 读回 reason 区分瞬时/额度类（同 GLM SSE 路径 fix-glm-ratelimit G3）。
				// 必须在下行 unmarkExhaustedForRetry 之前读（unmark 后读回空串）。
				// 只有通用 "rate_limited" 与 GLM 瞬时类（1302/1303/1305）保留同 key 重试；
				// GLM 额度类（1308/1310/unknown）、forwardRequest 已识别的余额/额度耗尽
				// （credit_balance_exhausted → quota_exceeded_*，#159 F3）与 GLM 账户级失效
				// （1113/1309，#159 F5）1 秒后不可能恢复，跳过同 key 重试直接换 key。
				// 注：极端并发下 reason 可能为 ""（key 恰被其他请求冷却恢复洗白），此时走 RETRY<2 同 key，
				// 与原行为一致，安全默认。
				reason := state.keyPool.exhaustReasonAt(keyIndex)
				skipSameKey := !sameKeyRetryable429Reason(reason)

				retryClientName := ""
				if principal != nil {
					retryClientName = principal.Name
				}

				// WP-3：RETRY<2 解除 exhausted 状态 + sleep 1s 重试同一个 key
				// 例外：额度/账户类 1 秒后不可能恢复，直接换 key（与 GLM SSE 分支对齐）。
				if !skipSameKey && keyFails < 2 {
					s.logger.Info("[keypool] Key failed (HTTP 429) but RETRY<2, sleep and retry same key",
						"request_id", appmiddleware.RequestIDFromContext(r.Context()),
						"target", target.Name,
						"client", retryClientName,
						"key_index", keyIndex,
						"key_fails", keyFails,
						"reason", reason,
					)
					// 解除 exhausted 状态让 selectKeyForClient 能重新选中该 key（WP-3 坚持机制）
					waitForKeyRetry(r.Context())
					if parentEnded() {
						break
					}
					state.keyPool.unmarkExhaustedForRetry(keyIndex)
					s.metrics.totalKeyRetries.Add(1)
					continue
				}
				// 换 key，全局计数+1（reqTotalFails 驱动 4 次全局安全阀）
				if parentEnded() {
					break
				}
				reqTotalFails++
				nextKey, nextIdx := state.keyPool.selectNextActiveKey(keyIndex)
				if nextKey == "" {
					// 所有 key 都被限流或耗尽：透传 429 给客户端。
					// 重新发起请求以获取新的 resp（因为已经 Close 了）
					// WP-2 C1 fix: all keys exhausted is an upstream rate-limit signal →
					// notify AIMD controller to decrease rate (OnLimit was previously dead code).
					if am := s.admissionManager.Load(); am != nil {
						am.OnLimit(target.Name)
					}
					if parentEnded() {
						break
					}
					resp, cancel, keyIndex, fErr = s.forwardRequest(r, state, forwardBody, upstreamModel, lease)
					break
				}
				if skipSameKey {
					s.logger.Info("[keypool] quota/account 429, skip same-key retry, switching key",
						"request_id", appmiddleware.RequestIDFromContext(r.Context()),
						"target", target.Name,
						"client", retryClientName,
						"key_index", keyIndex,
						"reason", reason,
						"next_key_index", nextIdx,
					)
				} else {
					s.logger.Info("[keypool] 429 rate limit, key RETRY>=2, switching to next key",
						"request_id", appmiddleware.RequestIDFromContext(r.Context()),
						"target", target.Name,
						"client", retryClientName,
						"key_fails", keyFails,
						"reason", reason,
						"prev_key_index", keyIndex,
						"next_key_index", nextIdx,
					)
				}
				if parentEnded() {
					break
				}
				s.metrics.totalKeyRetries.Add(1)
				continue
			}
			if state.keyPool == nil || keyIndex < 0 {
				break
			}
			// HIGH-1: 只有当当前 key 真正被标记耗尽（reason != ""）才换 key；通用 5xx / 4xx
			// （非 key 问题）换 key 无意义，直接透传，避免多 key 目标的无界 key 轮换风暴。
			reason := state.keyPool.exhaustReasonAt(keyIndex)
			if reason == "" {
				break
			}
			// fix-C 收敛：同一请求内连续 ≥2 个不同 key 返回"相同"账户级硬失败码（强同账户信号），
			// 说明换 key 无望，停止逐 key 重试，透传真实上游响应——既避免把整个池全部标记耗尽 +
			// 触发唤醒风暴，也让客户端看到真实的 401 而非笼统 503。
			// 评审回归修复（LOW-8910）：仅按相同 reason 收敛。不同 reason 可能来自多账户混合池的
			// 不同账户，不应收敛，否则会误 abandon 健康 key + 误 mute 整个 target。
			if isAccountLevelExhaustion(reason) {
				if reason == convergeCode {
					convergeStreak++
				} else {
					convergeCode = reason
					convergeStreak = 1
				}
				if convergeStreak >= 2 {
					accountLevelFailure = true
					s.logger.Warn("[keypool] convergence: multiple keys returned same account-level failure, passing through upstream response",
						"request_id", appmiddleware.RequestIDFromContext(r.Context()),
						"target", target.Name,
						"reason", reason,
						"keys_tried", convergeStreak,
					)
					break // 保持 resp 打开 → 交由 writeResponse 透传真实上游响应（按失败计）
				}
			}
			// 检查是否有下一个可用 key（当前 key 已在 forwardRequest 中被标记耗尽）
			if parentEnded() {
				break
			}
			nextKey, nextIdx := state.keyPool.selectKey()
			if nextKey == "" || nextIdx == keyIndex {
				break
			}
			// 判断是否为绝境探测：nextIdx 对应的 key 仍处于 exhausted 状态
			state.keyPool.mu.Lock()
			if nextIdx >= 0 && nextIdx < len(state.keyPool.entries) && state.keyPool.entries[nextIdx].exhausted {
				desperationProbe = true
			} else {
				desperationProbe = false
			}
			state.keyPool.mu.Unlock()
			// 有新 key 可用，关闭当前响应，重试
			resp.Body.Close()
			resp = nil
			if cancel != nil {
				cancel()
				cancel = nil
			}
			retryClientName := ""
			if principal != nil {
				retryClientName = principal.Name
			}
			s.logger.Info("[keypool] retrying with next key",
				"request_id", appmiddleware.RequestIDFromContext(r.Context()),
				"target", target.Name,
				"client", retryClientName,
				"prev_key_index", keyIndex,
				"next_key_index", nextIdx,
			)
			if parentEnded() {
				break
			}
			s.metrics.totalKeyRetries.Add(1)
		}
		parentEnded()

		// 恢复原始 Content-Type（支持重试时路由到其他 target，如 Azure 需原始 multipart）
		if origContentType != "" {
			r.Header.Set("Content-Type", origContentType)
		}
		if fErr != nil {
			deferCancel(cancel)

			fe, ok := fErr.(*forwardAttemptError)
			status := http.StatusServiceUnavailable // 默认服务不可用（5xx）；forwardAttemptError 通常带明确 status
			if ok && fe.status != 0 {
				status = fe.status
			}
			if ok && fe.noUsage != nil {
				pendingNoUsage = fe.noUsage
			}
			lastForwardStatus = status // 追踪状态码，failover 耗尽后 selectTarget 失败时传播
			lastMinimaxFailure = errors.Is(fErr, errMinimaxInBandError)
			// 429 时记录 quota 信息，供 selectTarget 失败路径使用（那时已脱离 keypool 上下文）
			if status == http.StatusTooManyRequests && state.keyPool != nil {
				lastQuotaInfo.isQuota, lastQuotaInfo.resetAt = state.keyPool.quotaExhaustionStatus()
			} else {
				lastQuotaInfo.isQuota = false
				lastQuotaInfo.resetAt = time.Time{}
			}

			// 旁路：上游网络错误 / 配置错误 → upstream-error.log
			if ok {
				writeForwardErrorLog(r, state, fe)
			}
			if err := r.Context().Err(); err != nil {
				finishContext(err, target.Name)
				return
			}

			if ok && fe.retryable {
				// LOW-67 评审回归修复：all-keys-exhausted（skipMute）由 keypool 冷却自管恢复，
				// 不在此 mute 目标——否则显式目标（allowFallback=false）会被 selectTarget 的
				// IsMuted 短路 60s，即便 key 已恢复也无法服务。retryable 仍允许 failover。
				if !fe.skipMute {
					s.handleForwardError(r, state, fe.err, status)
				}
				if !allowFallback {
					// WP-1: 全池耗尽（status=429）时，区分限额/限流返回体（用已记录的 quota 信息）
					if status == http.StatusTooManyRequests {
						writeUpstreamLimitedResponse(w, lastQuotaInfo.isQuota, lastQuotaInfo.resetAt)
					} else {
						http.Error(w, http.StatusText(status), status)
					}
					s.metrics.totalFailures.Add(1)
					requestOutcomeRecorded = true
					return
				}

				s.logger.Info("retrying request with alternate target",
					"request_id", appmiddleware.RequestIDFromContext(r.Context()),
					"failed_target", target.Name,
					"status", status,
					"attempt", attempt,
				)
				if err := r.Context().Err(); err != nil {
					finishContext(err, target.Name)
					return
				}
				s.metrics.totalTargetRetries.Add(1)
				continue
			}

			// 客户端在响应返回前主动断开（context.Canceled → 499）：
			// 上游链路本身没故障，故不计 totalFailures、不 mute 目标、不写错误体（客户端已走）。
			// 单独计数 + Warn 日志，让运维一眼区分"下游断开"与"上游/代理真故障"。
			// WriteHeader 仅为让 access log 记录 499；底层连接已断，写失败被 net/http 忽略。
			if status == StatusClientClosedRequest {
				finishContext(context.Canceled, target.Name)
				return
			}

			if ok {
				s.logger.Error("forward request failed",
					"request_id", appmiddleware.RequestIDFromContext(r.Context()),
					"target", target.Name,
					"error", errorlog.ErrorCategory(fe.err),
					"status", status,
				)
			} else {
				s.logger.Error("forward request failed",
					"request_id", appmiddleware.RequestIDFromContext(r.Context()),
					"target", target.Name,
					"error", errorlog.ErrorCategory(fErr),
					"status", status,
				)
			}
			// WP-1: 全池耗尽（status=429）时，区分限额/限流返回体
			if status == http.StatusTooManyRequests {
				writeUpstreamLimitedResponse(w, lastQuotaInfo.isQuota, lastQuotaInfo.resetAt)
			} else {
				http.Error(w, http.StatusText(status), status)
			}
			s.metrics.totalFailures.Add(1)
			requestOutcomeRecorded = true
			return
		}

		// A response writeResponse observes owns the request's usage
		// settlement. A passed-through non-2xx response is never observed or
		// billed, so an earlier sent attempt without official usage keeps its
		// pending zero-usage event (recorded by the deferred settlement).
		if responseObservesUsage(resp) {
			pendingNoUsage = nil
		}
		completion = s.writeResponse(w, r, state, resp, cancel, attempt, upstreamModel, forwardBody, startedAt, keyIndex, accountLevelFailure, lease)
		// 更新 IP 粘连：仅在粘连命中（刷新 TTL）或首次轮询（建立粘连）时更新。
		// Failover（原粘连目标暂不可用）和显式指定目标时不更新，避免劫持粘连。
		// 粘连 key 使用 upstreamModel（真实上游模型名，保留原始大小写），让不同 alias
		// 指向同一上游的请求归到同一条粘连记录，也与 selectTarget 中 Get side 的上游
		// 查找 key 保持一致。
		// MED-3: 收敛透传的账户级失败不应刷新/建立粘连到该坏目标。
		if !accountLevelFailure && principal != nil && target != nil {
			now := time.Now()
			targetName := strings.ToLower(target.Name)
			if selKind == selectionAffinityHit || selKind == selectionRoundRobin {
				s.affinity.Set(affinityKey(clientIP(r), principal.Name, upstreamModel), targetName, now)
			}
			// Session writes are independent of the final selection kind: refresh
			// the original target, or rebind only if it is permanently invalid.
			// A temporary forwarding failover must not even refresh its TTL.
			if sessionKey != "" && (sessionBoundTarget == "" || strings.EqualFold(sessionBoundTarget, targetName) || s.sessionTargetInvalid(sessionBoundTarget, allowed, model)) {
				s.affinity.SetWithTTL(sessionKey, targetName, now, sessionAffinityTTL)
			}
		}
		requestOutcomeRecorded = true
		return
	}
}

// Both HTTP 429 and GLM transient rejection backoffs belong to the parent
// request, not the already-ended forwarding attempt.
func waitForKeyRetry(ctx context.Context) {
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

func readAndBufferBody(r *http.Request) ([]byte, error) {
	if r.Body == nil || r.Body == http.NoBody {
		return nil, nil
	}

	data, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxRequestBodyBytes {
		return nil, fmt.Errorf("request body exceeds %d bytes limit", maxRequestBodyBytes)
	}
	if err := r.Body.Close(); err != nil {
		return nil, err
	}
	r.Body = io.NopCloser(bytes.NewReader(data))
	return data, nil
}

func deferCancel(cancel context.CancelFunc) {
	if cancel != nil {
		cancel()
	}
}

// writeQuotaExceededResponse 渲染 429 超限响应（OpenAI 兼容 JSON）。
// 字段：error.message / error.type / error.code / error.quota.{dimension, limit_usd, used_usd, resets_at}。
// 额度体系 v2：scope=job_level 表示客户端未绑定职级（未配置额度），渲染专属文案。
func writeQuotaExceededResponse(w http.ResponseWriter, info quota.ExceededInfo) {
	// Calibration unavailable is a 503: do not claim exhaustion or a reset time.
	if info.Scope == quota.ScopeAccounting {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{
				"message": "Usage accounting is temporarily unavailable. Please retry later.",
				"type":    "service_unavailable",
				"code":    "accounting_unavailable",
			},
		})
		return
	}
	quotaBody := map[string]any{
		"scope":     info.Scope,
		"dimension": info.Dimension,
		"limit_usd": info.Limit,
		"used_usd":  info.Used,
		"resets_at": info.ResetsAt.UTC().Format(time.RFC3339),
	}
	message := fmt.Sprintf("Quota exceeded (%s). Limit: $%.2f, used: $%.2f. Resets at %s",
		info.Dimension, info.Limit, info.Used, info.ResetsAt.UTC().Format(time.RFC3339))
	switch info.Scope {
	case "model_group":
		quotaBody["group_name"] = info.GroupName
	case quota.ScopeJobLevel:
		message = "No job level assigned: quota is not configured for this client. Contact the administrator to bind a job level."
	}
	body := map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    "quota_exceeded",
			"code":    "quota_exceeded",
			"quota":   quotaBody,
		},
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusTooManyRequests)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(body)
}

// writeUpstreamLimitedResponse 渲染上游限流/限额 429 响应（OpenAI 兼容 JSON）。
// 区分两类语义，让客户端 SDK 能正确决策重试策略：
//   - upstream_quota_exhausted：额度耗尽（1308 时限额/1310 周限额），带 resets_at + Retry-After，客户端应等待而非立即重试
//   - upstream_rate_limited：频率/QPS 限流（1302/1303），短 Retry-After，客户端可指数退避重试
func writeUpstreamLimitedResponse(w http.ResponseWriter, isQuota bool, resetAt time.Time) {
	w.Header().Set("Content-Type", "application/json")
	var body map[string]any
	if isQuota && !resetAt.IsZero() {
		retryAfter := int(time.Until(resetAt).Seconds())
		if retryAfter < 1 {
			retryAfter = 1
		}
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
		body = map[string]any{
			"error": map[string]any{
				"message":   "Upstream quota exhausted. Please retry after the reset time.",
				"type":      "upstream_quota_exhausted",
				"code":      "upstream_quota_exhausted",
				"resets_at": resetAt.UTC().Format(time.RFC3339),
			},
		}
	} else {
		w.Header().Set("Retry-After", "5")
		body = map[string]any{
			"error": map[string]any{
				"message": "Upstream rate limit exceeded. Please retry shortly.",
				"type":    "upstream_rate_limited",
				"code":    "upstream_rate_limited",
			},
		}
	}
	w.WriteHeader(http.StatusTooManyRequests)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(body)
}

// defaultQuietPeriod 返回失败静默基线冷却（S4 breakerCooldown 的 base；60s）。
func defaultQuietPeriod() time.Duration { return 60 * time.Second }

// normalizeRequestMembers applies normalizeProtectedMembers to a client body.
// When members are removed it logs member names and counts only, never
// values, and counts the request once.
func (s *Service) normalizeRequestMembers(r *http.Request, body []byte) ([]byte, bool) {
	normalized := body
	var report []memberNormalization
	// multipart/form-data and urlencoded fields follow the same rule, on the
	// body kind extractModel reads (a JSON object sent as urlencoded is JSON).
	// The JSON pass always runs; it leaves non-JSON bodies unchanged.
	if r != nil {
		switch kind, boundary, _ := classifyRequestBody(r.Header.Get("Content-Type"), body); {
		case kind == bodyMultipart && boundary != "":
			normalized, report = normalizeMultipartMembers(normalized, boundary)
		case kind == bodyURLEncoded:
			// urlencoded forms are forwarded as is (no JSON conversion): only
			// the removed pairs change.
			normalized, report = normalizeURLEncodedMembers(normalized)
		}
	}
	var jsonReport []memberNormalization
	normalized, jsonReport = normalizeProtectedMembers(normalized)
	report = append(report, jsonReport...)
	if report == nil {
		return body, false
	}
	s.metrics.totalRequestMembersNormalized.Add(1)
	s.logNormalizedMembers(r, report)
	return normalized, true
}

func (s *Service) logNormalizedMembers(r *http.Request, report []memberNormalization) {
	members := make([]string, 0, len(report))
	removed := 0
	for _, entry := range report {
		members = append(members, entry.Member+":"+strconv.Itoa(entry.Removed))
		removed += entry.Removed
	}
	requestID, path := "", ""
	if r != nil {
		requestID = appmiddleware.RequestIDFromContext(r.Context())
		path = r.URL.Path
	}
	s.logger.Info("request members normalized",
		"request_id", requestID,
		"path", path,
		"members", strings.Join(members, ","),
		"removed", removed,
	)
}

// MetricsSnapshot returns a copy of the service-level metrics counters.
func (s *Service) MetricsSnapshot() ServiceMetrics {
	keyRetries := s.metrics.totalKeyRetries.Load()
	targetRetries := s.metrics.totalTargetRetries.Load()
	return ServiceMetrics{
		TotalRequests:                        s.metrics.totalRequests.Load(),
		TotalSuccess:                         s.metrics.totalSuccess.Load(),
		TotalFailures:                        s.metrics.totalFailures.Load(),
		TotalUsageRecordFailures:             s.metrics.totalUsageRecordFailures.Load(),
		TotalUsageClientGoneNoUsage:          s.metrics.totalUsageClientGoneNoUsage.Load(),
		TotalUsageUpstreamInterruptedNoUsage: s.metrics.totalUsageUpstreamInterruptedNoUsage.Load(),
		TotalRequestMembersNormalized:        s.metrics.totalRequestMembersNormalized.Load(),
		TotalRetries:                         keyRetries + targetRetries,
		TotalKeyRetries:                      keyRetries,
		TotalTargetRetries:                   targetRetries,
		TotalClientCanceled:                  s.metrics.totalClientCanceled.Load(),
		ActiveRequests:                       s.metrics.activeRequests.Load(),
		StartTime:                            s.startTime,
		ResponsesWS:                          s.responsesWS.metricsSnapshot(),
	}
}

// TargetStatuses provides a read-only snapshot of target health information.
func (s *Service) TargetStatuses(now time.Time) []TargetStatus {
	snapshot := s.targetSnapshot()
	statuses := make([]TargetStatus, 0, len(snapshot))
	for _, state := range snapshot {
		if state == nil {
			continue
		}
		target := state.Target()
		if target == nil {
			continue
		}
		stats := state.Stats()
		endpoint := ""
		if target.Endpoint != nil {
			endpoint = target.Endpoint.String()
		}

		// 统计 key pool 状态
		var totalKeys, activeKeys, exhaustedSubKeys, exhaustedAPIKeys, rateLimitedKeys, blockedKeys, otherExhaustedKeys int
		if state.keyPool != nil {
			keyStatuses := state.keyPool.status()
			totalKeys = len(keyStatuses)
			for _, ks := range keyStatuses {
				switch {
				case ks.Blocked:
					blockedKeys++
				case !ks.Exhausted:
					activeKeys++
				case isCoolingExpiredAndRecoverable(ks):
					// 冷却已过期且 reason 可恢复：逻辑上可用，计入 activeKeys，
					// 与 status() 的"可用(待激活)"显示口径对齐（消除"冷却过期但 ActiveKeys=0"误告警）。
					activeKeys++
				case ks.ExhaustReason == "quota_exceeded_subscription":
					exhaustedSubKeys++
				case ks.ExhaustReason == "quota_exceeded_api":
					exhaustedAPIKeys++
				case ks.ExhaustReason == "rate_limited":
					rateLimitedKeys++
				case isGLMRateLimitReason(ks.ExhaustReason):
					// GLM 各类限流（5h 时限额 / 1310 周限额 / 1302 QPS / unknown 兜底）
					// 本质都是限流（冷却后恢复），计入 rateLimitedKeys 保持与"限流"语义一致。
					// 前端概览页会按 reason 前缀进一步细分为 GLM 额度 / GLM 频率。
					rateLimitedKeys++
				default:
					// 其他失效原因：invalid_token / billing_error /
					// account_disabled / 旧版 quota_exceeded 等，均计入此处
					// 以保证各分类之和等于 totalKeys。
					otherExhaustedKeys++
				}
			}
		} else if target.APIKey != "" {
			// 单 key 模式
			totalKeys = 1
			activeKeys = 1
		}

		statuses = append(statuses, TargetStatus{
			Name:                      target.Name,
			EndpointType:              target.EndpointType,
			Endpoint:                  endpoint,
			Paused:                    target.Paused,
			Muted:                     state.IsMuted(now),
			MutedUntil:                stats.MutedUntil,
			LastSuccess:               stats.LastSuccess,
			LastFailure:               stats.LastFailure,
			ConsecutiveFailures:       stats.ConsecutiveFailure,
			TotalSuccessRequests:      stats.TotalSuccess,
			TotalFailedRequests:       stats.TotalFailure,
			TotalKeys:                 totalKeys,
			ActiveKeys:                activeKeys,
			ExhaustedSubscriptionKeys: exhaustedSubKeys,
			ExhaustedAPIKeys:          exhaustedAPIKeys,
			RateLimitedKeys:           rateLimitedKeys,
			BlockedKeys:               blockedKeys,
			OtherExhaustedKeys:        otherExhaustedKeys,
			Capacity:                  target.MaxConcurrency,
			InFlight:                  state.inFlightCount(),
			Phase:                     state.Phase(now),
			AffinityHits:              state.affinityHitCount(),
			BreakerTrips:              state.breakerTripCount(),
			Window15m:                 state.runtimeWindow.snapshot(now),
		})
		if s := &statuses[len(statuses)-1]; target.MaxConcurrency > 0 {
			s.Utilization = float64(s.InFlight) / float64(target.MaxConcurrency)
		}
	}
	return statuses
}

// StartTime returns the instant the service was constructed.
func (s *Service) StartTime() time.Time {
	return s.startTime
}

// KeyPoolStatus returns the key pool status for a target, or nil if no pool.
func (s *Service) KeyPoolStatus(targetName string) []KeyStatus {
	s.mu.RLock()
	state, ok := s.targetsByName[strings.ToLower(targetName)]
	s.mu.RUnlock()
	if !ok || state == nil || state.keyPool == nil {
		return nil
	}
	return state.keyPool.status()
}

// TargetKeySnapshot compares stored slots with the current runtime under the
// service lock. An index is nil when the slot has no runtime key-pool entry.
func (s *Service) TargetKeySnapshot(target config.Target) (uint64, []KeyStatus, []*int, bool) {
	name := strings.ToLower(strings.TrimSpace(target.Name))
	expected, _, err := buildTargetStates([]config.Target{target})
	if err != nil || expected[name] == nil {
		return 0, nil, nil, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	state := s.targetsByName[name]
	keys := mergeTargetKeys(target)
	if state == nil || !slices.Equal(keys, state.target.APIKeys) {
		return 0, nil, nil, false
	}
	// Pool pause is materialized into runtime and need not equal target.Paused.
	expectedTarget := *expected[name].target
	runtimeTarget := *state.target
	expectedTarget.Paused = runtimeTarget.Paused
	// Persistence omits empty top-level collections; nested JSON values retain their shape.
	if len(expectedTarget.ModelMappings) == 0 {
		expectedTarget.ModelMappings = nil
	}
	if len(runtimeTarget.ModelMappings) == 0 {
		runtimeTarget.ModelMappings = nil
	}
	if len(expectedTarget.CustomHeaders) == 0 {
		expectedTarget.CustomHeaders = nil
	}
	if len(runtimeTarget.CustomHeaders) == 0 {
		runtimeTarget.CustomHeaders = nil
	}
	if len(expectedTarget.CustomBody) == 0 {
		expectedTarget.CustomBody = nil
	}
	if len(runtimeTarget.CustomBody) == 0 {
		runtimeTarget.CustomBody = nil
	}
	expectedBody, expectedErr := json.Marshal(expectedTarget.CustomBody)
	runtimeBody, runtimeErr := json.Marshal(runtimeTarget.CustomBody)
	expectedTarget.CustomBody, runtimeTarget.CustomBody = nil, nil
	if expectedErr != nil || runtimeErr != nil || !bytes.Equal(expectedBody, runtimeBody) || !reflect.DeepEqual(&expectedTarget, &runtimeTarget) {
		return 0, nil, nil, false
	}
	indices := make([]*int, 1+len(target.APIKeys))
	if state.keyPool == nil {
		return s.targetRevision, nil, indices, true
	}
	if len(state.keyPool.entries) != len(keys) {
		return 0, nil, nil, false
	}
	byKey := make(map[string]int, len(keys))
	for i, key := range keys {
		if state.keyPool.entries[i].key != key {
			return 0, nil, nil, false
		}
		byKey[key] = i
	}
	for i, key := range append([]string{target.APIKey}, target.APIKeys...) {
		if index, ok := byKey[strings.TrimSpace(key)]; ok {
			index := index
			indices[i] = &index
		}
	}
	return s.targetRevision, state.keyPool.status(), indices, true
}

// SetKeyBlockedIfTargetRevision binds an indexed operation to the observed
// runtime revision and performs it before that revision can be replaced.
func (s *Service) SetKeyBlockedIfTargetRevision(targetName string, index int, blocked bool, revision uint64) error {
	if revision == 0 {
		return ErrTargetSnapshotChanged
	}
	return s.setKeyBlocked(targetName, index, blocked, revision)
}

func (s *Service) setKeyBlocked(targetName string, index int, blocked bool, revision uint64) error {
	s.mu.Lock()
	if revision != 0 && revision != s.targetRevision {
		s.mu.Unlock()
		return ErrTargetSnapshotChanged
	}
	state := s.targetsByName[strings.ToLower(targetName)]
	if state == nil || state.keyPool == nil {
		s.mu.Unlock()
		return fmt.Errorf("target %q not found or has no key pool", targetName)
	}
	if index < 0 || index >= len(state.keyPool.entries) {
		s.mu.Unlock()
		return fmt.Errorf("key index %d out of range", index)
	}
	if blocked {
		state.keyPool.blockKey(index)
	} else {
		state.keyPool.unblockKey(index)
	}
	s.advanceTargetRevisionLocked()
	s.mu.Unlock()
	if blocked && s.responsesWS != nil {
		s.responsesWS.invalidate(false)
	}
	return nil
}

// BlockKey manually blocks a specific key in the target's key pool.
func (s *Service) BlockKey(targetName string, index int) error {
	return s.setKeyBlocked(targetName, index, true, 0)
}

// UnblockKey manually unblocks a specific key in the target's key pool.
func (s *Service) UnblockKey(targetName string, index int) error {
	return s.setKeyBlocked(targetName, index, false, 0)
}

// WakeUpKeys triggers the wake-up model for a target's key pool.
// Returns the number of keys recovered, or an error if the target has no key pool.
func (s *Service) WakeUpKeys(targetName string) (recovered int, err error) {
	s.mu.RLock()
	state, ok := s.targetsByName[strings.ToLower(targetName)]
	s.mu.RUnlock()
	if !ok || state == nil || state.keyPool == nil {
		return 0, fmt.Errorf("target %q not found or has no key pool", targetName)
	}

	now := time.Now()
	if !state.keyPool.tryWakeUp(now) {
		return 0, fmt.Errorf("wake-up already in progress or in cooldown")
	}
	defer state.keyPool.wakeUpComplete()

	// 唤醒策略（按 reason 分流）：
	// - 纯手动 blocked 的 key：直接解除屏蔽（用户主动唤醒 = 明确要恢复），
	//   不依赖探测——探测端点（/v1/models）并非所有 provider 都支持，
	//   用探测来决定是否解除手动屏蔽会导致唤醒按钮永远无效。
	// - exhausted + GLM 瞬时类（1302/1303/1305）：直接 markRecovered，跳过 /v1/models 探测。
	//   原因：智谱 /v1/models 端点拼接后会成 v4/v1/models（404），探测永远失败；
	//   即使探测返回 200 也无法证明瞬时过载窗口已过。
	// - exhausted + GLM 额度类（1308/1310/unknown）：**不强制复活**，交给冷却定时器。
	//   额度类强制复活后下一个请求必然再撞限、再标 5h，唤醒按钮变成"点了闪一下又灰"，
	//   用户观感更差。
	// - exhausted + GLM 账户级失效（1113/1309）：直接 markRecovered（人工充值/续订后的明确恢复）。
	// - exhausted + 非 GLM reason：保留既有的 probeKey → markRecovered 路径。
	// 注意：自动唤醒路径（forward.go::wakeUpProbe）不受影响——已通过
	// getProbeableExhaustedKeys + isHardFailureReason 排除 GLM reason，本次不动。
	ctx := context.Background()
	exhaustedKeys := state.keyPool.getExhaustedKeys()
	for _, idx := range exhaustedKeys {
		if state.keyPool.isBlockedOnly(idx) {
			// 纯手动屏蔽：直接解除，无需探测
			state.keyPool.unblockKey(idx)
			recovered++
			continue
		}
		reason := state.keyPool.exhaustReasonAt(idx)
		if isGLMAccountLevelReason(reason) {
			// GLM 账户级失效（1113 欠费 / 1309 套餐到期，#159 F5）：永久屏蔽，冷却不会到期，
			// 自动探测也排除它们；智谱 /models 探测不可靠（同上）。运维充值/续订后点唤醒
			// 是明确的人工恢复，直接 markRecovered；若仍未恢复，下一个真实请求会重新永久屏蔽。
			if state.keyPool.markRecovered(idx) {
				recovered++
			}
			continue
		}
		if isGLMRateLimitReason(reason) {
			if isTransientGLMReason(reason) {
				// GLM 瞬时类（1302/1303/1305）：直接恢复，跳过无效探测
				if state.keyPool.markRecovered(idx) {
					recovered++
				}
			}
			// GLM 额度类（1308/1310/unknown）：不复活，交给冷却定时器
			continue
		}
		if !s.probeKey(ctx, state, idx) {
			continue // 探测失败，key 仍无效，保持 exhausted
		}
		if state.keyPool.markRecovered(idx) {
			recovered++
		}
	}

	return recovered, nil
}

func (s *Service) setRequestTimeout(timeout time.Duration) {
	timeout = normalizeRequestTimeout(timeout)
	s.requestTimeout.Store(timeout.Nanoseconds())
}

func (s *Service) getRequestTimeout() time.Duration {
	nanos := s.requestTimeout.Load()
	if nanos <= 0 {
		return normalizeRequestTimeout(0)
	}
	return time.Duration(nanos)
}

func normalizeRequestTimeout(timeout time.Duration) time.Duration {
	if timeout <= 0 {
		return 1800 * time.Second
	}
	return timeout
}

// GetTrace 按 request_id 查询单条 trace 记录（DEBUG 模式）。
func (s *Service) GetTrace(requestID string) *tracestore.TraceRecord {
	if s.traceStore == nil {
		return nil
	}
	return s.traceStore.Get(requestID)
}

// ListTrace 列出最近的 trace 记录（DEBUG 模式），支持 offset 分页。
func (s *Service) ListTrace(offset, limit int) []*tracestore.TraceRecord {
	if s.traceStore == nil {
		return nil
	}
	return s.traceStore.List(offset, limit)
}

// TraceEnabled 报告 trace store 是否在保存记录；不触碰磁盘。
func (s *Service) TraceEnabled() bool {
	return s.traceStore.Enabled()
}

// TraceStats 返回 trace store 的统计信息（DEBUG 模式）。
func (s *Service) TraceStats() map[string]int64 {
	if s.traceStore == nil {
		return nil
	}
	return s.traceStore.Stats()
}

// Close cancels and drains hijacked WebSocket turns before flushing traces.
// It should be called after server.Shutdown.
func (s *Service) Close() error {
	if s.responsesWS != nil {
		s.responsesWS.close()
	}
	if err := s.closeResponsesWebsockets(); err != nil {
		return err
	}
	if s.traceStore == nil {
		return nil
	}
	return s.traceStore.Close()
}

// initAdmissionTargetLocked preserves live AIMD buckets and derives capacity
// from current entries. Lock order: Service.mu -> keyPoolDomain.mu -> admission.
func (s *Service) initAdmissionTargetLocked(manager *admission.Manager, name string, state *targetState, register bool) {
	if state == nil || state.Target() == nil {
		return
	}
	pool := state.keyPool
	if pool == nil {
		total := len(state.Target().APIKeys)
		if register {
			manager.InitTarget(name, total)
		}
		manager.UpdateActiveKeys(name, total, total)
		return
	}
	s.bindAdmissionPool(name, pool)
	pool.mu.Lock()
	defer pool.mu.Unlock()
	total := len(pool.entries)
	if register {
		manager.InitTarget(name, total)
	}
	manager.UpdateActiveKeys(name, pool.activeKeyCountLocked(), total)
}

// bindAdmissionPool accepts old-generation notifications in a surviving domain,
// but never trusts their old counts/indexes. keyPool calls this outside its lock.
func (s *Service) bindAdmissionPool(name string, pool *keyPool) {
	pool.setOnKeyChange(func(_, _ int) {
		s.mu.RLock()
		defer s.mu.RUnlock()
		state := s.targetsByName[name]
		if state == nil || state.keyPool == nil || state.keyPool.keyPoolDomain != pool.keyPoolDomain {
			return
		}
		current := state.keyPool
		current.mu.Lock()
		defer current.mu.Unlock()
		if manager := s.admissionManager.Load(); manager != nil {
			manager.UpdateActiveKeys(name, current.activeKeyCountLocked(), len(current.entries))
		}
	})
}
