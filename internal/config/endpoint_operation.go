package config

import (
	"net/url"
	"strings"
)

// endpointOperationSuffixes 是可识别的标准接口路径后缀。上游文档常直接给出
// 完整接口地址（如 https://host/v2/chat/completions）；endpoint 路径以其中之一
// 结尾时，后缀之前的部分才是 base。判据只看路径形态，不看 host 或厂商。
//
// 顺序有意义：/chat/completions 须排在 /completions 之前。
var endpointOperationSuffixes = []string{
	"/chat/completions",
	"/images/generations",
	"/images/edits",
	"/completions",
	"/responses",
	"/embeddings",
	"/messages",
}

// SplitEndpointOperationPath 把以标准接口后缀结尾的 endpoint 路径拆成 base 与
// 后缀（小写规范形式）。匹配大小写不敏感，容忍末尾的 "/"。不以接口后缀结尾时
// 原样返回路径，operation 为空。
func SplitEndpointOperationPath(path string) (base, operation string) {
	trimmed := strings.TrimRight(path, "/")
	lower := strings.ToLower(trimmed)
	for _, suffix := range endpointOperationSuffixes {
		if strings.HasSuffix(lower, suffix) {
			return trimmed[:len(trimmed)-len(suffix)], suffix
		}
	}
	return path, ""
}

// EndpointTypeAcceptsOperationURL 报告该 endpoint_type 是否把完整接口地址按 base
// 处理。只有按 base 拼接客户端路径的策略（透传、双协议）适用；Azure 的路径
// 自带 deployment 结构，终态 URL 类型的 endpoint 本身就是完整地址，均不适用。
func EndpointTypeAcceptsOperationURL(code string) bool {
	meta, ok := EndpointTypeMetaOf(code)
	if !ok {
		return false
	}
	return meta.PathStrategy == ProviderPathPassthrough || meta.PathStrategy == ProviderPathDual
}

// EffectiveEndpoint 返回运行时实际使用的 base 地址，以及被识别并去掉的接口后缀。
// endpoint 不含接口后缀、类型不适用或无法解析时原样返回，operation 为空。
// 代理构造目标与管理端提示共用此函数。
func EffectiveEndpoint(endpointType, endpoint string) (effective, operation string) {
	if !EndpointTypeAcceptsOperationURL(endpointType) {
		return endpoint, ""
	}
	parsed, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return endpoint, ""
	}
	base, op := SplitEndpointOperationPath(parsed.Path)
	if op == "" {
		return endpoint, ""
	}
	parsed.Path = base
	parsed.RawPath = ""
	return parsed.String(), op
}
