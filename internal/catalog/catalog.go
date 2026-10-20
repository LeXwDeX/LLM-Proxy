// Package catalog 提供嵌入式模型元数据目录，包括模型默认费用、展示名、别名等。
//
// 数据来源于 models.dev/api.json（通过 scripts/update-model-catalog.py 转换），
// 运行时不依赖任何外部 URL，通过 go:embed 嵌入 data/models.json。
//
// 费用数据为估算参考值，可能与实际计费存在偏差。
package catalog

import (
	"embed"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

//go:embed data/models.json
var embeddedData embed.FS

// EndpointType 常量统一在 internal/config 包中定义（config.EndpointTypeAzureOpenAI 等），
// 本包不再重复定义，避免同步维护风险。

// ModelEntry 是模型目录中的一条记录。
type ModelEntry struct {
	EndpointType    string   `json:"endpoint_type"`
	Model           string   `json:"model"`
	DisplayName     string   `json:"display_name,omitempty"`
	Aliases         []string `json:"aliases,omitempty"`
	Capabilities    []string `json:"capabilities,omitempty"`
	DefaultCost     *Cost    `json:"default_cost,omitempty"`
	ContextWindow   int      `json:"context_window,omitempty"`
	MaxOutputTokens int      `json:"max_output_tokens,omitempty"`
	MaxInputTokens  int      `json:"max_input_tokens,omitempty"`
	ModelFamily     string   `json:"model_family,omitempty"`
}

// Cost 表示模型默认费用（每 1M tokens，单位 USD）。
// 数据为估算参考值，来源于公开定价信息。
type Cost struct {
	InputPer1MTokens            float64  `json:"input_per_1m_tokens"`
	OutputPer1MTokens           float64  `json:"output_per_1m_tokens"`
	CacheWritePer1MToken        float64  `json:"cache_write_per_1m_tokens"`
	CacheReadPer1MToken         float64  `json:"cache_read_per_1m_tokens"`
	ImageInputPer1MTokens       *float64 `json:"image_input_per_1m_tokens,omitempty"`
	CachedImageInputPer1MTokens *float64 `json:"cached_image_input_per_1m_tokens,omitempty"`
	ImageOutputPer1MTokens      *float64 `json:"image_output_per_1m_tokens,omitempty"`
}

// UnmarshalJSON supports both the new cache_write_per_1m_tokens key and the
// legacy cached_input_per_1m_tokens key for backward compatibility with
// existing embedded catalog data. The new key takes priority; the legacy key
// is only consulted when the new key is absent or zero.
func (c *Cost) UnmarshalJSON(data []byte) error {
	type alias Cost
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*c = Cost(a)
	if c.CacheWritePer1MToken == 0 {
		raw := map[string]json.RawMessage{}
		if json.Unmarshal(data, &raw) == nil {
			if v, ok := raw["cached_input_per_1m_tokens"]; ok {
				var f float64
				if json.Unmarshal(v, &f) == nil {
					c.CacheWritePer1MToken = f
				}
			}
		}
	}
	return nil
}

// Catalog 是内存中的模型目录，支持按 endpoint_type + model 精确查找和别名查找。
type Catalog struct {
	mu      sync.RWMutex
	entries []ModelEntry
	// 索引: "endpoint_type:normalized_model" -> *ModelEntry
	index map[string]*ModelEntry
	// 别名索引: "endpoint_type:normalized_alias" -> normalized_model
	aliasIndex map[string]string
}

// New 创建并加载嵌入的模型目录。
func New() (*Catalog, error) {
	data, err := embeddedData.ReadFile("data/models.json")
	if err != nil {
		return nil, fmt.Errorf("catalog: read embedded data: %w", err)
	}
	return newFromData(data)
}

// newFromData 从 JSON 字节创建 Catalog（也供测试使用）。
func newFromData(data []byte) (*Catalog, error) {
	var entries []ModelEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("catalog: parse data: %w", err)
	}

	c := &Catalog{
		entries:    entries,
		index:      make(map[string]*ModelEntry, len(entries)),
		aliasIndex: make(map[string]string),
	}

	for i := range entries {
		e := &entries[i]
		e.Model = strings.ToLower(strings.TrimSpace(e.Model))
		e.EndpointType = strings.ToLower(strings.TrimSpace(e.EndpointType))

		key := e.EndpointType + ":" + e.Model
		c.index[key] = e

		for _, alias := range e.Aliases {
			alias = strings.ToLower(strings.TrimSpace(alias))
			if alias != "" && alias != e.Model {
				aliasKey := e.EndpointType + ":" + alias
				c.aliasIndex[aliasKey] = e.Model
			}
		}
	}

	return c, nil
}

// Lookup 按 endpoint_type + model 查找模型信息。
// 先尝试精确匹配，再尝试别名匹配。查找不区分大小写。
func (c *Catalog) Lookup(endpointType, model string) *ModelEntry {
	c.mu.RLock()
	defer c.mu.RUnlock()

	endpointType = strings.ToLower(strings.TrimSpace(endpointType))
	model = strings.ToLower(strings.TrimSpace(model))

	key := endpointType + ":" + model
	if entry, ok := c.index[key]; ok {
		return entry
	}

	// 尝试别名
	if canonical, ok := c.aliasIndex[key]; ok {
		canonicalKey := endpointType + ":" + canonical
		if entry, ok := c.index[canonicalKey]; ok {
			return entry
		}
	}

	return nil
}

// LookupDefaultCost 查找指定模型的默认费用。
func (c *Catalog) LookupDefaultCost(endpointType, model string) *Cost {
	entry := c.Lookup(endpointType, model)
	if entry == nil {
		return nil
	}
	return entry.DefaultCost
}

// ListByEndpointType 返回指定 endpoint_type 的所有模型（副本）。
func (c *Catalog) ListByEndpointType(endpointType string) []ModelEntry {
	c.mu.RLock()
	defer c.mu.RUnlock()

	endpointType = strings.ToLower(strings.TrimSpace(endpointType))
	var result []ModelEntry
	for _, e := range c.entries {
		if e.EndpointType == endpointType {
			result = append(result, e)
		}
	}
	return result
}

// ListAll 返回所有模型条目的副本。
func (c *Catalog) ListAll() []ModelEntry {
	c.mu.RLock()
	defer c.mu.RUnlock()
	result := make([]ModelEntry, len(c.entries))
	copy(result, c.entries)
	return result
}

// ResolveAlias 尝试将别名解析为规范模型名。
// 如果 model 不是别名，则原样返回（小写化后）。
func (c *Catalog) ResolveAlias(endpointType, model string) string {
	c.mu.RLock()
	defer c.mu.RUnlock()

	endpointType = strings.ToLower(strings.TrimSpace(endpointType))
	model = strings.ToLower(strings.TrimSpace(model))

	key := endpointType + ":" + model
	if canonical, ok := c.aliasIndex[key]; ok {
		return canonical
	}
	return model
}
