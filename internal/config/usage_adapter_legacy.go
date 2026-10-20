package config

import (
	"errors"
	"net/url"
	"regexp"
	"strings"
)

var legacyAliyunWorkspaceHost = regexp.MustCompile(`^[a-z0-9-]+\.(cn-beijing|cn-shanghai|ap-southeast-1|us-east-1)\.maas\.aliyuncs\.com$`)

type usageAdapterInput struct {
	value   string
	present bool
}

// UsageAdapterImportInput returns the original file value and field presence
// for migration auditing. If a caller changed the selection after loading,
// the current value wins. Programmatic empty targets have no JSON field.
func (t Target) UsageAdapterImportInput() (value string, present bool) {
	if input := t.usageAdapterInput; input != nil && NormalizeUsageAdapter(input.value) == NormalizeUsageAdapter(t.UsageAdapter) {
		return input.value, input.present
	}
	return t.UsageAdapter, t.UsageAdapter != ""
}

// LegacyUsageAdapterForEndpoint converts old automatic selections during
// migration only. Runtime policy must never infer an adapter from an endpoint.
func LegacyUsageAdapterForEndpoint(endpoint string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil || !u.IsAbs() || u.Hostname() == "" {
		// URL parse errors can include credentials; do not propagate the URL.
		return "", errors.New("legacy usage_adapter endpoint must be an absolute URL with a hostname")
	}
	host := strings.ToLower(u.Hostname())
	switch host {
	case "dashscope.aliyuncs.com", "dashscope-intl.aliyuncs.com", "dashscope-us.aliyuncs.com":
		return UsageAdapterQwen, nil
	case "open.bigmodel.cn", "api.z.ai":
		return UsageAdapterGLM, nil
	case "api.deepseek.com":
		return UsageAdapterDeepSeek, nil
	}
	if legacyAliyunWorkspaceHost.MatchString(host) {
		return UsageAdapterQwen, nil
	}
	return UsageAdapterStandard, nil
}
