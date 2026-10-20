package proxy

import (
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	"math"
	"net/http"
	"strings"
	"testing"
)

func TestImageUsageDimensions(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		incomplete bool
	}{
		{"valid", `{"input_tokens":100,"output_tokens":20,"input_tokens_details":{"text_tokens":60,"image_tokens":40,"cached_tokens":10,"cached_tokens_details":{"image_tokens":5}},"output_tokens_details":{"image_tokens":20}}`, false},
		{"missing cache split", `{"input_tokens":100,"output_tokens":20,"input_tokens_details":{"text_tokens":60,"image_tokens":40,"cached_tokens":10}}`, true},
		{"too many images", `{"input_tokens":100,"output_tokens":20,"input_tokens_details":{"image_tokens":101}}`, true},
		{"text mismatch", `{"input_tokens":100,"output_tokens":20,"input_tokens_details":{"text_tokens":50,"image_tokens":40}}`, true},
		{"cached mismatch", `{"input_tokens":100,"output_tokens":20,"input_tokens_details":{"image_tokens":40,"cached_tokens":10,"cached_tokens_details":{"image_tokens":11}}}`, true},
		{"audio", `{"input_tokens":100,"output_tokens":20,"input_tokens_details":{"audio_tokens":1}}`, true},
		{"negative image", `{"input_tokens":100,"output_tokens":20,"input_tokens_details":{"image_tokens":-1}}`, true},
		{"fractional image", `{"input_tokens":100,"output_tokens":20,"input_tokens_details":{"image_tokens":1.5}}`, true},
		{"output exceeds total", `{"input_tokens":100,"output_tokens":20,"output_tokens_details":{"image_tokens":21}}`, true},
		{"missing totals", `{"input_tokens_details":{"image_tokens":40}}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := usageParser{protocol: config.ProtocolOpenAIResponses}
			got, _, ok := p.extractTokens("application/json", []byte(`{"usage":`+tc.body+`}`))
			if !ok || got.costIncomplete != tc.incomplete {
				t.Fatalf("usage=%+v found=%v", got, ok)
			}
		})
	}
}

func TestImagePartitionsUseFinalAdapterTotals(t *testing.T) {
	imageRate, cachedRate := 10.0, 3.0
	rates := usage.CostRates{InputPer1MTokens: 2, CacheReadPer1MToken: 1, ImageInputPer1MTokens: &imageRate, CachedImageInputPer1MTokens: &cachedRate}
	for _, adapter := range []string{"kimi", "deepseek", "qwen", "glm", "standard"} {
		for _, split := range []struct {
			name, details      string
			image, cachedImage int64
			incomplete         bool
			cost               float64
		}{
			{"missing", `"text_tokens":60,"image_tokens":40`, 40, 0, true, .000510},
			{"explicit zero", `"text_tokens":60,"image_tokens":40,"cached_tokens_details":{"image_tokens":0}`, 40, 0, false, .000510},
			{"explicit nonzero", `"text_tokens":60,"image_tokens":40,"cached_tokens_details":{"image_tokens":5}`, 35, 5, false, .000480},
			{"text cache split", `"text_tokens":60,"image_tokens":40,"cached_tokens_details":{"text_tokens":7}`, 37, 3, false, .000492},
			{"pure image", `"text_tokens":0,"image_tokens":100`, 90, 10, false, .000930},
			{"pure text", `"text_tokens":100,"image_tokens":0`, 0, 0, false, .000190},
		} {
			for _, stream := range []bool{false, true} {
				t.Run(adapter+"/"+split.name+map[bool]string{false: "/json", true: "/sse"}[stream], func(t *testing.T) {
					details, alias := split.details, ""
					switch adapter {
					case "kimi":
						alias = `,"cached_tokens":10`
					case "deepseek":
						alias = `,"prompt_cache_hit_tokens":10,"prompt_cache_miss_tokens":90`
					default:
						details += `,"cached_tokens":10`
					}
					if adapter == "qwen" {
						// Qwen discards the base cache_write_tokens field and uses
						// cache_creation_input_tokens instead. Images must be
						// checked against the resulting input=90, not input=20.
						details += `,"cache_write_tokens":70,"cache_creation_input_tokens":0`
					}
					body := `{"usage":{"prompt_tokens":100,"completion_tokens":0,"prompt_tokens_details":{` + details + `}` + alias + `}}`
					p := usageParser{protocol: config.ProtocolOpenAIChat, usageAdapter: adapter, requestBody: []byte(`{"messages":[]}`)}
					var got usageTokens
					if stream {
						got, _, _ = p.extractSSE([]byte("data: " + body + "\n\ndata: [DONE]\n\n"))
					} else {
						got, _, _ = p.extractTokens("application/json", []byte(body))
					}
					if got.InputTokens != 90 || got.CachedTokens != 10 || got.ImageInputTokens != split.image || got.CachedImageInputTokens != split.cachedImage || got.costIncomplete != split.incomplete || got.status != usageStatusComplete {
						t.Fatalf("usage=%+v", got)
					}
					if split.incomplete && !strings.Contains(got.diagnostic, "missing_cached_image_input_tokens") || !split.incomplete && got.diagnostic != "" {
						t.Fatalf("diagnostic=%q", got.diagnostic)
					}
					cost := rates.CostFor(got.InputTokens, got.OutputTokens, got.CachedTokens, got.CacheCreationTokens, got.ExplicitCacheReadTokens, got.CacheCreation5mTokens, got.CacheCreation1hTokens, got.ImageInputTokens, got.CachedImageInputTokens, got.ImageOutputTokens)
					if math.Abs(cost-split.cost) > 1e-10 {
						t.Fatalf("cost=%v want=%v", cost, split.cost)
					}
				})
			}
		}
	}
}

func TestImagePartitionsUseFinalQwenResponsesTotals(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "json", true: "sse"}[stream], func(t *testing.T) {
			body := `{"usage":{"input_tokens":100,"output_tokens":0,"input_tokens_details":{"text_tokens":60,"image_tokens":40,"cached_tokens":10,"cache_write_tokens":70,"cached_tokens_details":{"image_tokens":5}}}}`
			p := usageParser{protocol: config.ProtocolOpenAIResponses, usageAdapter: "qwen", requestHeaders: http.Header{}}
			var got usageTokens
			if stream {
				got, _, _ = p.extractSSE([]byte(`data: {"type":"response.completed","response":` + body + "}\n\ndata: [DONE]\n\n"))
			} else {
				got, _, _ = p.extractTokens("application/json", []byte(body))
			}
			if got.InputTokens != 90 || got.ImageInputTokens != 35 || got.CachedImageInputTokens != 5 || got.costIncomplete || got.status != usageStatusComplete || got.diagnostic != "" {
				t.Fatalf("usage=%+v", got)
			}
		})
	}
}

func TestImageCachedPartitionPresence(t *testing.T) {
	for _, tc := range []struct {
		name, details string
		cachedImage   int64
		incomplete    bool
	}{
		{"missing", `"text_tokens":60,"image_tokens":40,"cached_tokens":10`, 0, true},
		{"explicit zero", `"text_tokens":60,"image_tokens":40,"cached_tokens":10,"cached_tokens_details":{"image_tokens":0}`, 0, false},
		{"explicit nonzero", `"text_tokens":60,"image_tokens":40,"cached_tokens":10,"cached_tokens_details":{"image_tokens":5}`, 5, false},
		{"text cache split", `"text_tokens":60,"image_tokens":40,"cached_tokens":10,"cached_tokens_details":{"text_tokens":7}`, 3, false},
		{"pure text absent image", `"cached_tokens":10`, 0, false},
		{"pure text zero image", `"text_tokens":100,"image_tokens":0,"cached_tokens":10`, 0, false},
		{"pure image", `"text_tokens":0,"image_tokens":100,"cached_tokens":10`, 10, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := usageParser{protocol: config.ProtocolOpenAIResponses}
			got, _, _ := p.extractTokens("application/json", []byte(`{"usage":{"input_tokens":100,"output_tokens":0,"input_tokens_details":{`+tc.details+`}}}`))
			if got.CachedImageInputTokens != tc.cachedImage || got.costIncomplete != tc.incomplete || tc.incomplete && !strings.Contains(got.diagnostic, "missing_cached_image_input_tokens") {
				t.Fatalf("usage=%+v", got)
			}
		})
	}
}

func TestImageOverageNormalizedByParser(t *testing.T) {
	p := usageParser{protocol: config.ProtocolOpenAIResponses}
	got, _, _ := p.extractTokens("application/json", []byte(`{"usage":{"input_tokens":100,"output_tokens":20,"input_tokens_details":{"image_tokens":200,"cached_tokens":10,"cached_tokens_details":{"image_tokens":30}},"output_tokens_details":{"image_tokens":40}}}`))
	if got.ImageInputTokens != 90 || got.CachedImageInputTokens != 10 || got.ImageOutputTokens != 20 || !got.costIncomplete || !strings.Contains(got.diagnostic, "image_partition_exceeds_total") {
		t.Fatalf("unnormalized image partitions: %+v", got)
	}
}

func TestImageOutputDetailsOverrideProtocolDefault(t *testing.T) {
	for _, tc := range []struct {
		name, details string
		image         int64
		incomplete    bool
	}{
		{"all image", `{"image_tokens":20}`, 20, false},
		{"mixed", `{"text_tokens":8,"image_tokens":12}`, 12, false},
		{"text only", `{"text_tokens":20,"image_tokens":0}`, 0, false},
		{"text split alone", `{"text_tokens":8}`, 12, false},
		{"mismatch", `{"text_tokens":8,"image_tokens":20}`, 20, true},
		{"overage", `{"image_tokens":21}`, 20, true},
		{"empty", `{}`, 0, true},
		{"null", `null`, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := usageParser{protocol: config.ProtocolOpenAIImage}
			got, _, _ := p.extractTokens("application/json", []byte(`{"usage":{"input_tokens":12,"output_tokens":20,"output_tokens_details":`+tc.details+`}}`))
			if got.ImageOutputTokens != tc.image || got.costIncomplete != tc.incomplete {
				t.Fatalf("usage=%+v", got)
			}
		})
	}
}

func TestImageExplicitCachePartitionsAreDisjoint(t *testing.T) {
	p := usageParser{protocol: config.ProtocolOpenAIResponses, usageAdapter: "qwen", requestHeaders: map[string][]string{"X-Dashscope-Session-Cache": {"enable"}}}
	got, _, _ := p.extractTokens("application/json", []byte(`{"usage":{"input_tokens":100,"output_tokens":0,"input_tokens_details":{"image_tokens":40,"cached_tokens":50,"cached_tokens_details":{"image_tokens":30}}}}`))
	if got.ExplicitCacheReadTokens != 20 || got.CachedImageInputTokens != 30 {
		t.Fatalf("overlapping image/text cache partitions: %+v", got)
	}
}

func TestImageDimensionsAcrossOpenAIProtocolsAndStream(t *testing.T) {
	base := `{"usage":{"input_tokens":100,"output_tokens":20,"input_tokens_details":{"text_tokens":60,"image_tokens":40,"cached_tokens":10,"cached_tokens_details":{"image_tokens":5}},"output_tokens_details":{"image_tokens":20}}}`
	for _, op := range []config.ProtocolType{config.ProtocolOpenAIChat, config.ProtocolOpenAIResponses, config.ProtocolOpenAIImage} {
		for _, chatKeys := range []bool{false, true} {
			if chatKeys && op == config.ProtocolOpenAIResponses {
				continue
			}
			body := base
			if chatKeys {
				body = strings.NewReplacer("input_tokens", "prompt_tokens", "output_tokens", "completion_tokens").Replace(body)
			}
			p := usageParser{protocol: op}
			jsonTokens, _, _ := p.extractTokens("application/json", []byte(body))
			streamBody := body
			if op == config.ProtocolOpenAIResponses {
				streamBody = `{"type":"response.completed","response":` + body + `}`
			}
			if op == config.ProtocolOpenAIImage {
				streamBody = `{"type":"image_generation.completed","usage":` + strings.TrimSuffix(strings.TrimPrefix(body, `{"usage":`), `}`) + `}`
			}
			streamTokens, _, _ := p.extractSSE([]byte("data: " + streamBody + "\n\ndata: [DONE]\n\n"))
			for _, got := range []usageTokens{jsonTokens, streamTokens} {
				if got.ImageInputTokens != 35 || got.CachedImageInputTokens != 5 || got.ImageOutputTokens != 20 || got.InputTokens != 90 || got.costIncomplete || got.fields&(usageFieldImageInput|usageFieldCachedImageInput|usageFieldImageOutput) != (usageFieldImageInput|usageFieldCachedImageInput|usageFieldImageOutput) {
					t.Fatalf("op=%s chatKeys=%v usage=%+v", op, chatKeys, got)
				}
			}
		}
	}
}
