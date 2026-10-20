// url.go — 上游 URL 拼装与查询参数清洗。
package proxy

import (
	"errors"
	"net/url"
	"regexp"
	"strings"
)

// endpointTrailingVersionRe 匹配路径末尾的版本段（/v1, /v4, /v1beta 等）。
// 用于判断 endpoint 是否已声明版本，避免客户端版本前缀叠加。
var endpointTrailingVersionRe = regexp.MustCompile(`(?:^|/)(v\d+[a-zA-Z]*)/?$`)

// clientLeadingVersionRe 匹配路径开头的版本段（/v1, /v1beta 等）。
var clientLeadingVersionRe = regexp.MustCompile(`^/v\d+[a-zA-Z]*`)

// versionOnlyPathRe 匹配「仅版本段」的 endpoint 路径（/v1, /v1/, /v1beta 等）。
// 用于识别终态 URL 类型下的级联 base 配置（见 isImageBaseEndpointPath）。
var versionOnlyPathRe = regexp.MustCompile(`^/v\d+[a-zA-Z]*/?$`)

func (s *Service) buildURL(target *Target, original *url.URL) (*url.URL, error) {
	if target == nil || target.Endpoint == nil {
		return nil, errors.New("target not configured")
	}

	forward := *target.Endpoint
	forward.RawQuery = ""
	forward.Fragment = ""

	registry := s.providerRegistry
	if registry == nil {
		if fallbackProviderRegistryError != nil {
			return nil, fallbackProviderRegistryError
		}
		registry = fallbackProviderRegistry
	}
	profile := registry.Lookup(target.EndpointType)
	if profile != nil && profile.Path == nil {
		return nil, errors.New("proxy: provider path strategy not configured")
	}

	// 终态 URL：不拼接客户端 path（如网宿图像通道）。
	// 客户端发 POST /v1/images/generations，但上游只认固定 URL，因此整体覆盖。
	//
	// 例外（级联部署）：endpoint 未配置具体路径（空 / 仅版本段，如
	// http://<下一跳>:8000/v1）时视为 base URL，透传客户端 path。
	// 下一跳是另一台 llms-proxy 等按路径路由的服务时，整体覆盖会丢弃
	// /images/* 路径导致下游路径检查失败（no target supports path）。
	// 真正的终态 URL（如 …/v1/<hash>/openai-image）不会命中此分支，行为不变。
	isTerminal := profile != nil && profile.Path != nil && profile.Path.IsTerminalURL()
	if isTerminal {
		if isImageBaseEndpointPath(forward.Path) {
			clientPath := stripRedundantClientVersion(forward.Path, original.Path)
			forward.Path = strings.TrimRight(forward.Path, "/") + "/" + strings.TrimLeft(clientPath, "/")
			forward.Path = deduplicateV1Path(forward.Path)
		}
		forward.RawQuery = normalizeForwardQuery(original)
		return &forward, nil
	}

	// 使用 PathMapper 改写路径
	var path string
	if profile != nil {
		pathMapper := profile.PathForTarget(target)
		// 准备 endpoint path
		forward.Path = pathMapper.PrepareEndpointPath(forward.Path)
		// 改写客户端路径。当 endpoint 已声明版本段（/v4, /v1 等）且客户端路径
		// 也带版本前缀（/v1/...）时，剥除客户端的版本前缀，避免版本段叠加
		// 导致路径污染（如 GLM: /v4/ + /v1/chat/completions → /v4/v1/chat/completions）。
		clientPath := stripRedundantClientVersion(forward.Path, original.Path)
		path = pathMapper.RewritePath(clientPath, "")
	} else {
		// 兼容旧逻辑（不应到达，因为所有 endpoint_type 都已注册）
		path = mergePaths("", original.Path)
	}

	// Concatenate paths explicitly instead of using url.URL.Parse, because
	// url.Parse treats paths starting with "/" as absolute and would discard
	// any sub-path already present in the endpoint (e.g. a gateway base path
	// like /v2/gws/<id>/anthropic).
	forward.Path = strings.TrimRight(forward.Path, "/") + "/" + strings.TrimLeft(path, "/")
	forward.Path = deduplicateV1Path(forward.Path)
	forward.RawQuery = normalizeForwardQuery(original)

	if profile != nil {
		if mapper, ok := profile.Path.(queryPathMapper); ok {
			forward.RawQuery = mapper.RewriteQuery(forward.Path, forward.RawQuery)
		}
	}

	return &forward, nil
}

// isImageBaseEndpointPath 判断终态 URL 类型 target 的 endpoint 路径是否为
// 「base 形态」（空、/ 或仅版本段如 /v1）——此时应透传客户端 path 而非覆盖。
// 安全性：真正的图像终态 URL（如网宿 …/openai-image）路径不可能仅为版本段，
// 而仅版本段的配置在覆盖语义下必然请求失败，故此分流不影响任何可用配置。
func isImageBaseEndpointPath(p string) bool {
	p = strings.TrimSpace(p)
	if p == "" || p == "/" {
		return true
	}
	return versionOnlyPathRe.MatchString(p)
}

// isDeploymentBasedPath 判断路径是否为 Azure 的 deployment-based 格式
// （如 /openai/deployments/gpt-4o/chat/completions），该格式需要 api-version 查询参数。
// v1 路径（如 /openai/v1/images/edits）不需要 api-version。
func isDeploymentBasedPath(path string) bool {
	return strings.Contains(strings.ToLower(path), "/deployments/")
}

func normalizeForwardQuery(original *url.URL) string {
	if original == nil {
		return ""
	}

	query := original.Query()
	deleteQueryKeyCaseInsensitive(query, "target")
	deleteQueryKeyCaseInsensitive(query, "api-version")
	deleteQueryKeyCaseInsensitive(query, "api_version")
	deleteQueryKeyCaseInsensitive(query, "api-key")
	deleteQueryKeyCaseInsensitive(query, "key") // Google Gemini native query auth

	return query.Encode()
}

// appendAzureAPIVersion 向已编码的 query string 追加 api-version 参数。
// 如果已存在 api-version（不应发生，因为 normalizeForwardQuery 会剥离），则不重复追加。
func appendAzureAPIVersion(rawQuery, version string) string {
	if rawQuery == "" {
		return "api-version=" + url.QueryEscape(version)
	}
	return rawQuery + "&api-version=" + url.QueryEscape(version)
}

func deleteQueryKeyCaseInsensitive(query url.Values, key string) {
	for existing := range query {
		if strings.EqualFold(existing, key) {
			delete(query, existing)
		}
	}
}

// isAnthropicStylePath 判断给定客户端 path 是否走 Anthropic API 形态。
// 路径以 /v1/messages 开头（包括 /v1/messages、/v1/messages/count_tokens 等子路径）
// 视为 Anthropic 形态。其余路径视为 OpenAI 兼容形态。
func isAnthropicStylePath(p string) bool {
	pl := strings.ToLower(strings.TrimSpace(p))
	return pl == "/v1/messages" || strings.HasPrefix(pl, "/v1/messages/") || strings.HasPrefix(pl, "/v1/messages?")
}

func isOpenAIResponsesStylePath(p string) bool {
	pl := strings.ToLower(strings.TrimSpace(p))
	return pl == "/v1/responses" || strings.HasPrefix(pl, "/v1/responses/") || strings.HasPrefix(pl, "/v1/responses?")
}

func ensureLeadingSlash(p string) string {
	if p == "" {
		return "/"
	}
	if p[0] != '/' {
		return "/" + p
	}
	return p
}

// stripRedundantClientVersion 当 endpoint 路径以版本段结尾（/v1, /v4, /v1beta 等）
// 且客户端路径以版本段开头（/v1/...）时，剥除客户端的版本前缀。
//
// 解决场景：上游 endpoint 已声明版本（如智谱 GLM 的 /api/paas/v4/），客户端 SDK
// 仍发送标准 /v1/chat/completions。若不剥除，会拼出 /v4/v1/chat/completions
// 导致上游 404。剥除后正确拼为 /v4/chat/completions。
//
// 安全性：仅当双方都带版本段时才剥除；endpoint 无版本段（如 Claude 根路径）
// 或客户端路径无版本前缀时原样返回，不影响标准 OpenAI/Azure/Claude/Gemini 用法。
func stripRedundantClientVersion(endpointPath, clientPath string) string {
	if endpointPath == "" || clientPath == "" {
		return clientPath
	}
	// endpoint 末尾必须是版本段（/v1, /v4, /v1beta 等）
	if !endpointTrailingVersionRe.MatchString(endpointPath) {
		return clientPath
	}
	// 客户端路径开头必须是版本段
	loc := clientLeadingVersionRe.FindStringIndex(clientPath)
	if loc == nil {
		return clientPath
	}
	// 剥除客户端的版本前缀，保留剩余路径
	rest := clientPath[loc[1]:]
	if rest == "" {
		return "/"
	}
	if !strings.HasPrefix(rest, "/") {
		rest = "/" + rest
	}
	return rest
}

func deduplicateV1Path(p string) string {
	// 处理路径中间的 /v1/v1/ 重复：保留第一个 /v1/，跳过第二个 /v1
	if idx := strings.Index(p, "/v1/v1/"); idx >= 0 {
		return p[:idx+1] + p[idx+4:]
	}
	// 处理路径以 /v1/v1 结尾（无后续路径）
	if strings.HasSuffix(p, "/v1/v1") {
		return p[:len(p)-3]
	}
	return p
}

func mergePaths(prefix, path string) string {
	if prefix == "" {
		if path == "" {
			return "/"
		}
		return path
	}
	if path == "" || path == "/" {
		return prefix
	}
	if strings.HasPrefix(path, prefix+"/") || path == prefix {
		return path
	}
	return strings.TrimRight(prefix, "/") + "/" + strings.TrimLeft(path, "/")
}
