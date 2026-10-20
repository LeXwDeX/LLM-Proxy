package config

import "testing"

// TestEndpointTypeMetaSingleSourceOfTruth 验证 ValidEndpointTypes 与 endpointTypes
// 元数据保持一致，防止后续有人在两处重复维护出现漂移。
func TestEndpointTypeMetaSingleSourceOfTruth(t *testing.T) {
	metas := AllEndpointTypeMetas()
	if len(metas) != len(ValidEndpointTypes) {
		t.Fatalf("ValidEndpointTypes(%d) 与 AllEndpointTypeMetas(%d) 长度不一致，存在漂移", len(ValidEndpointTypes), len(metas))
	}
	for i, m := range metas {
		if m.Code != ValidEndpointTypes[i] {
			t.Errorf("索引 %d 不一致：meta.Code=%q ValidEndpointTypes=%q", i, m.Code, ValidEndpointTypes[i])
		}
		if m.DisplayName == "" {
			t.Errorf("%s 缺少 DisplayName（UI 必需）", m.Code)
		}
		if m.ShortLabel == "" {
			t.Errorf("%s 缺少 ShortLabel（UI 必需）", m.Code)
		}
		if m.BadgeBackground == "" || m.BadgeForeground == "" {
			t.Errorf("%s 缺少徽章配色", m.Code)
		}
	}
}

func TestEndpointTypeMetaOf(t *testing.T) {
	// 标准 code 命中
	m, ok := EndpointTypeMetaOf("openai_image")
	if !ok {
		t.Fatal("未找到 openai_image 元数据")
	}
	if m.DisplayName != "OpenAI Image" {
		t.Errorf("DisplayName 不符合预期：%s", m.DisplayName)
	}
	// 大小写 + 空白容错
	if _, ok := EndpointTypeMetaOf("  OPENAI_IMAGE  "); !ok {
		t.Error("大小写/空白未正规化")
	}
	// 未知类型返回 false
	if _, ok := EndpointTypeMetaOf("unknown_type"); ok {
		t.Error("未知类型应返回 false")
	}
}

// TestRequiresResourcePathPrefixOnlyAzure 防止有人误把 RequiresResourcePathPrefix
// 标记到非 azure_openai 上（业务上目前仅 azure_openai 需要）。
func TestRequiresResourcePathPrefixOnlyAzure(t *testing.T) {
	for _, m := range AllEndpointTypeMetas() {
		if m.RequiresResourcePathPrefix && m.Code != EndpointTypeAzureOpenAI {
			t.Errorf("%s 不应需要 RequiresResourcePathPrefix（目前仅 azure_openai 需要）", m.Code)
		}
	}
}

func TestEndpointMetadataDoesNotLeakAliases(t *testing.T) {
	meta, _ := EndpointTypeMetaOf(EndpointTypeOpenAI)
	original := meta.Aliases[0]
	defer func() { meta.Aliases[0] = original }()
	meta.Aliases[0] = "caller-mutated"
	if got := NormalizeEndpointType(original); got != EndpointTypeOpenAI {
		t.Fatalf("metadata consumer changed alias normalization: %q", got)
	}
}

// The admin form shows the Responses WebSocket preference from this flag while
// the server and proxy apply it to canonical openai only; keep them aligned.
func TestOnlyOpenAISupportsPreferWebSocket(t *testing.T) {
	for _, meta := range AllEndpointTypeMetas() {
		if got, want := meta.SupportsPreferWebSocket, meta.Code == EndpointTypeOpenAI; got != want {
			t.Fatalf("%s supports_prefer_websocket=%v, want %v", meta.Code, got, want)
		}
	}
}
