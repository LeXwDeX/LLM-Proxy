// provider_profile.go — ProviderProfile 接口、AuthStrategy / PathMapper 策略与 ProviderRegistry。
// config 声明原生协议与兼容能力，本层将策略标识绑定到可执行策略。
package proxy

import (
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

// ---------------------------------------------------------------------------
// AuthStrategy — 上游认证头注入策略
// ---------------------------------------------------------------------------

// AuthStrategy 定义上游认证头注入策略。
type AuthStrategy interface {
	// InjectAuth 向上游请求注入认证头。
	// clientPath 是客户端原始路径（用于条件性注入 anthropic-version）。
	InjectAuth(req *http.Request, apiKey string, clientPath string) error
}

// BearerAuth 设置 Authorization: Bearer <key>
type BearerAuth struct{}

func (a *BearerAuth) InjectAuth(req *http.Request, apiKey string, clientPath string) error {
	req.Header.Set("Authorization", "Bearer "+apiKey)
	return nil
}

// AnthropicAuth 设置 x-api-key + anthropic-version（或 Bearer 模式）
type AnthropicAuth struct {
	BearerMode bool // 当 AuthMode=="bearer" 时使用 Bearer 而非 x-api-key
}

func (a *AnthropicAuth) InjectAuth(req *http.Request, apiKey string, clientPath string) error {
	if a.BearerMode {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	} else {
		req.Header.Set("x-api-key", apiKey)
	}
	if req.Header.Get("anthropic-version") == "" {
		req.Header.Set("anthropic-version", "2023-06-01")
	}
	return nil
}

// GeminiAuth 设置 x-goog-api-key
type GeminiAuth struct{}

func (a *GeminiAuth) InjectAuth(req *http.Request, apiKey string, clientPath string) error {
	req.Header.Set("x-goog-api-key", apiKey)
	return nil
}

// BearerWithConditionalAnthropicVersion 设置 Bearer + 条件性 anthropic-version。
// 用于 dual_protocol：Bearer 认证，但 Anthropic 路径需要 anthropic-version。
type BearerWithConditionalAnthropicVersion struct{}

func (a *BearerWithConditionalAnthropicVersion) InjectAuth(req *http.Request, apiKey string, clientPath string) error {
	req.Header.Set("Authorization", "Bearer "+apiKey)
	if isAnthropicStylePath(clientPath) {
		if req.Header.Get("anthropic-version") == "" {
			req.Header.Set("anthropic-version", "2023-06-01")
		}
	}
	return nil
}

// AzureAuth 设置 api-key 或 Bearer passthrough
type AzureAuth struct {
	AllowBearer    bool
	AzureAuthValue string // 来自客户端 X-Azure-Authorization 头
}

func (a *AzureAuth) InjectAuth(req *http.Request, apiKey string, clientPath string) error {
	if a.AllowBearer && a.AzureAuthValue != "" {
		req.Header.Set("Authorization", a.AzureAuthValue)
	} else {
		req.Header.Set("api-key", apiKey)
	}
	return nil
}

// ---------------------------------------------------------------------------
// PathMapper — 客户端路径到上游路径的改写策略
// ---------------------------------------------------------------------------

// PathMapper 定义客户端路径到上游路径的改写策略。
type PathMapper interface {
	// RewritePath 返回改写后的上游路径。
	RewritePath(clientPath string, resourcePathPrefix string) string
	// IsTerminalURL 返回 true 表示 endpoint URL 是终态 URL（不拼接客户端 path）。
	IsTerminalURL() bool
	// PrepareEndpointPath 返回经过 provider 特定处理后的 endpoint path。
	// 默认返回原值。
	PrepareEndpointPath(endpointPath string) string
}

// PassthroughPath 不做路径改写（仅 mergePaths）。
type PassthroughPath struct{}

func (m *PassthroughPath) RewritePath(clientPath, resourcePathPrefix string) string {
	return mergePaths(resourcePathPrefix, clientPath)
}

func (m *PassthroughPath) IsTerminalURL() bool { return false }

func (m *PassthroughPath) PrepareEndpointPath(endpointPath string) string { return endpointPath }

// DualProtocolPath 双协议路径改写：从 target 读取 prefix 配置，按路径自动分流。
// OpenAI 风格路径加 OpenAIPrefix，Anthropic 风格路径加 AnthropicPrefix。
type DualProtocolPath struct {
	OpenAIPrefix      string
	AnthropicPrefix   string
	SupportsResponses bool // 兼容旧构造字段；Responses 能力开关由 Target.SupportsPath 校验
}

func (m *DualProtocolPath) RewritePath(clientPath, resourcePathPrefix string) string {
	path := mergePaths(resourcePathPrefix, clientPath)
	if isAnthropicStylePath(clientPath) {
		path = m.AnthropicPrefix + ensureLeadingSlash(path)
	} else {
		path = m.OpenAIPrefix + ensureLeadingSlash(path)
	}
	return path
}

func (m *DualProtocolPath) IsTerminalURL() bool { return false }

func (m *DualProtocolPath) PrepareEndpointPath(endpointPath string) string { return endpointPath }

// TerminalURLPath 终态 URL（不拼接客户端 path）。
type TerminalURLPath struct{}

func (m *TerminalURLPath) RewritePath(clientPath, resourcePathPrefix string) string {
	return "" // 不使用
}

func (m *TerminalURLPath) IsTerminalURL() bool { return true }

func (m *TerminalURLPath) PrepareEndpointPath(endpointPath string) string { return endpointPath }

// ---------------------------------------------------------------------------
// BodyPolicy — 请求体预处理策略
// ---------------------------------------------------------------------------

// BodyPolicy 定义请求体预处理策略。
type BodyPolicy struct {
	// PreserveMultipart 为 true 时保留 multipart/form-data 不转换。
	// Azure 和网宿图像端点原生支持 multipart。
	PreserveMultipart bool

	// SanitizeFunc 可选的请求体净化函数。
	// 仅 Azure 使用：白名单过滤不兼容字段。
	// 返回净化后的 body 和被剥离的字段名列表。
	SanitizeFunc func(r *http.Request, body []byte) ([]byte, []string)
}

// ---------------------------------------------------------------------------
// ProviderProfile — provider 的协议能力与策略描述
// ---------------------------------------------------------------------------

// ProviderProfile 描述一个 provider 的协议能力和策略。
type ProviderProfile struct {
	EndpointType        string
	Provider            string
	Protocols           []config.ProtocolType
	Auth                AuthStrategy
	Path                PathMapper
	RequestSchemas      []config.RequestSchema // 兼容透传能力，独立于原生 Protocols
	SupportedPaths      []string               // nil = 不限制（由协议决定）；非 nil = 仅支持这些路径后缀
	probePath           string
	probeUsesMappedPath bool
	Body                BodyPolicy
}

// SupportsProtocol 检查 provider 是否支持给定协议。
func (p *ProviderProfile) SupportsProtocol(protocol config.ProtocolType) bool {
	for _, proto := range p.Protocols {
		if proto == protocol {
			return true
		}
	}
	return false
}

// SupportsPath 检查 provider 是否支持给定客户端路径。
// 内置类型读取 config 声明的兼容 schema；扩展 profile 未声明 schema 时按原生协议判断。
// 再检查可选 SupportedPaths 限制，不替代入口的严格路径校验。
func (p *ProviderProfile) SupportsPath(path string) bool {
	if p.RequestSchemas != nil {
		schema := requestSchemaForPath(path)
		supported := false
		for _, candidate := range p.RequestSchemas {
			if candidate == schema {
				supported = true
				break
			}
		}
		if !supported {
			return false
		}
	} else if !p.SupportsProtocol(ResolveProtocol(path)) {
		return false
	}
	if p.SupportedPaths == nil {
		return true
	}
	pathLower := strings.ToLower(path)
	for _, sp := range p.SupportedPaths {
		if strings.HasSuffix(pathLower, sp) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// ProviderRegistry — endpoint_type → ProviderProfile 映射
// ---------------------------------------------------------------------------

// ProviderRegistry 将 endpoint_type 映射到 ProviderProfile。
type ProviderRegistry struct {
	profiles map[string]*ProviderProfile
}

// NewProviderRegistry 创建空的 ProviderRegistry。
func NewProviderRegistry() *ProviderRegistry {
	return &ProviderRegistry{profiles: make(map[string]*ProviderProfile)}
}

// Lookup 返回给定 endpoint_type 的 ProviderProfile，未找到返回 nil。
func (r *ProviderRegistry) Lookup(endpointType string) *ProviderProfile {
	if r == nil {
		return nil
	}
	return r.profiles[endpointType]
}

// Register 注册一个 ProviderProfile。
func (r *ProviderRegistry) Register(profile *ProviderProfile) error {
	if r == nil {
		return errors.New("proxy: provider registry not configured")
	}
	if profile == nil || strings.TrimSpace(profile.EndpointType) == "" {
		return errors.New("proxy: provider profile requires endpoint type")
	}
	if nilProviderStrategy(profile.Auth) || nilProviderStrategy(profile.Path) {
		return fmt.Errorf("proxy: provider %q requires auth and path strategies", profile.EndpointType)
	}
	if len(profile.Protocols) == 0 && len(profile.RequestSchemas) == 0 {
		return fmt.Errorf("proxy: provider %q requires protocol or schema capabilities", profile.EndpointType)
	}
	for _, protocol := range profile.Protocols {
		if !config.IsValidProtocolType(protocol) {
			return fmt.Errorf("proxy: provider %q has invalid protocol %q", profile.EndpointType, protocol)
		}
	}
	for _, schema := range profile.RequestSchemas {
		if !config.IsValidRequestSchema(schema) {
			return fmt.Errorf("proxy: provider %q has invalid request schema %q", profile.EndpointType, schema)
		}
	}
	if r.profiles == nil {
		r.profiles = make(map[string]*ProviderProfile)
	}
	r.profiles[profile.EndpointType] = profile
	return nil
}

func nilProviderStrategy(strategy any) bool {
	if strategy == nil {
		return true
	}
	value := reflect.ValueOf(strategy)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

// DefaultProviderRegistry preserves the original convenience API. Invalid
// built-in declarations publish no profiles; service startup uses the error
// returning constructor below to report the precise configuration fault.
func DefaultProviderRegistry() *ProviderRegistry {
	registry, err := NewDefaultProviderRegistry()
	if err != nil {
		return NewProviderRegistry()
	}
	return registry
}

// NewDefaultProviderRegistry validates every built-in profile before publishing
// the registry. NewService returns these errors before starting request handling.
func NewDefaultProviderRegistry() (*ProviderRegistry, error) {
	return providerRegistryFromMetadata(config.AllEndpointTypeMetas())
}

func providerRegistryFromMetadata(metas []config.EndpointTypeMeta) (*ProviderRegistry, error) {
	r := NewProviderRegistry()
	for _, meta := range metas {
		if len(meta.Protocols) == 0 || len(meta.RequestSchemas) == 0 {
			return nil, fmt.Errorf("proxy: provider %q requires native protocols and request schemas", meta.Code)
		}
		if r.Lookup(meta.Code) != nil {
			return nil, fmt.Errorf("proxy: duplicate provider %q", meta.Code)
		}
		profile := &ProviderProfile{
			EndpointType: meta.Code, Provider: meta.Provider,
			probePath: meta.ProbePath, probeUsesMappedPath: meta.ProbeUsesMappedPath,
			Protocols: meta.Protocols, RequestSchemas: meta.RequestSchemas,
			Body: BodyPolicy{PreserveMultipart: meta.PreserveMultipart},
		}
		switch meta.AuthStrategy {
		case config.ProviderAuthBearer:
			profile.Auth = &BearerAuth{}
		case config.ProviderAuthAzure:
			profile.Auth = &AzureAuth{}
		case config.ProviderAuthAnthropic:
			profile.Auth = &AnthropicAuth{}
		case config.ProviderAuthGemini:
			profile.Auth = &GeminiAuth{}
		case config.ProviderAuthDual:
			profile.Auth = &BearerWithConditionalAnthropicVersion{}
		default:
			return nil, fmt.Errorf("proxy: provider %q has unknown auth strategy %q", meta.Code, meta.AuthStrategy)
		}
		switch meta.PathStrategy {
		case config.ProviderPathPassthrough:
			profile.Path = &PassthroughPath{}
		case config.ProviderPathAzure:
			profile.Path = &AzurePath{}
		case config.ProviderPathTerminal:
			profile.Path = &TerminalURLPath{}
		case config.ProviderPathDual:
			profile.Path = &DualProtocolPath{}
		default:
			return nil, fmt.Errorf("proxy: provider %q has unknown path strategy %q", meta.Code, meta.PathStrategy)
		}
		if meta.SanitizeAzureBody {
			profile.Body.SanitizeFunc = sanitizeRequestBodyForAzure
		}
		if err := r.Register(profile); err != nil {
			return nil, err
		}
	}
	return r, nil
}

// Target binding is performed by the strategy, keeping provider-specific
// configuration out of the generic HTTP forwarder and URL assembler.
type targetAuthStrategy interface {
	ForTarget(*Target, *http.Request) AuthStrategy
}
type targetPathMapper interface{ ForTarget(*Target) PathMapper }
type queryPathMapper interface {
	RewriteQuery(path, rawQuery string) string
}

func (p *ProviderProfile) AuthForTarget(target *Target, request *http.Request) AuthStrategy {
	if binding, ok := p.Auth.(targetAuthStrategy); ok {
		return binding.ForTarget(target, request)
	}
	return p.Auth
}
func (p *ProviderProfile) PathForTarget(target *Target) PathMapper {
	if binding, ok := p.Path.(targetPathMapper); ok {
		return binding.ForTarget(target)
	}
	return p.Path
}
func (a *AnthropicAuth) ForTarget(target *Target, request *http.Request) AuthStrategy {
	return &AnthropicAuth{BearerMode: target.AuthMode == config.AuthModeBearer}
}
func (a *AzureAuth) ForTarget(target *Target, request *http.Request) AuthStrategy {
	value := ""
	if request != nil {
		value = strings.TrimSpace(request.Header.Get(headerAzureAuthorization))
	}
	return &AzureAuth{AllowBearer: target.AllowBearer, AzureAuthValue: value}
}
func (a *AzureAuth) ValidateCredential(apiKey string) error {
	if apiKey == "" && (!a.AllowBearer || a.AzureAuthValue == "") {
		return errors.New("proxy: missing azure credential; provide api key or X-Azure-Authorization")
	}
	return nil
}
func (m *DualProtocolPath) ForTarget(target *Target) PathMapper {
	return &DualProtocolPath{OpenAIPrefix: target.OpenAIPrefix, AnthropicPrefix: target.AnthropicPrefix, SupportsResponses: target.SupportsResponses}
}

// AzurePath adds the deployment API version after generic query cleaning.
// Azure v1 requests intentionally omit that query parameter.
type AzurePath struct{ PassthroughPath }

func (m *AzurePath) RewriteQuery(path, rawQuery string) string {
	if isDeploymentBasedPath(path) {
		return appendAzureAPIVersion(rawQuery, "2026-03-01-preview")
	}
	return rawQuery
}

func (s *Service) resolveAuthStrategy(target *Target, request *http.Request) AuthStrategy {
	if s.providerRegistry == nil || target == nil {
		return nil
	}
	profile := s.providerRegistry.Lookup(target.EndpointType)
	if profile == nil {
		return nil
	}
	return profile.AuthForTarget(target, request)
}

// Only URL fallback consumers use this private immutable registry. Services
// keep their own registry so integrations can register additional profiles.
var fallbackProviderRegistry, fallbackProviderRegistryError = NewDefaultProviderRegistry()
