package proxy

import "github.com/LeXwDeX/LLM-Proxy/internal/config"

func openAIUsageKeys(m map[string]any, op config.ProtocolType) (input, output, details, outDetails string) {
	input, output, details, outDetails = "prompt_tokens", "completion_tokens", "prompt_tokens_details", "completion_tokens_details"
	// The Images API reports input/output keys; compatible image upstreams may
	// still use the Chat Completions keys.
	_, imageKeys := m["input_tokens"]
	_, chatKeys := m["prompt_tokens"]
	if op == config.ProtocolOpenAIResponses || imageKeys && (op != config.ProtocolOpenAIChat || !chatKeys) {
		input, output, details, outDetails = "input_tokens", "output_tokens", "input_tokens_details", "output_tokens_details"
	}
	return input, output, details, outDetails
}

// Completion and Responses totals already contain reasoning and cache subsets.
// Read the base totals here; image partitions are finalized after adapter patches.
func parseOpenAIUsage(m map[string]any, op config.ProtocolType) usageTokens {
	var t usageTokens
	input, output, details, outDetails := openAIUsageKeys(m, op)
	total := t.count(m, input, usageFieldInput)
	t.OutputTokens = t.count(m, output, usageFieldOutput)
	d := usageDetails(m, details)
	t.CachedTokens = t.count(d, "cached_tokens", usageFieldCached)
	t.CacheCreationTokens = t.count(d, "cache_write_tokens", usageFieldCacheCreation)
	t.ReasoningTokens = t.count(usageDetails(m, outDetails), "reasoning_tokens", usageFieldReasoning)
	if t.ReasoningTokens > t.OutputTokens {
		t.conflict("reasoning_exceeds_output")
	}
	t.partition(total)
	return t
}

// Adapter patches may supply cache aliases or replace cache-write counts. Read
// image observations from the original snapshot only after those final totals
// are known, so no valid cache-image count is prematurely diagnosed or clamped.
func (t *usageTokens) finalizeOpenAIImagePartitions(m map[string]any, op config.ProtocolType) {
	input, _, details, outDetails := openAIUsageKeys(m, op)
	total := t.count(m, input, usageFieldInput)
	d := usageDetails(m, details)
	image := t.count(d, "image_tokens", usageFieldImageInput)
	cachedDetails := usageDetails(d, "cached_tokens_details")
	t.CachedImageInputTokens = t.count(cachedDetails, "image_tokens", usageFieldCachedImageInput)
	_, cachedImagePresent := cachedDetails["image_tokens"]
	if !cachedImagePresent && image > 0 && t.CachedTokens > 0 {
		if _, textPresent := cachedDetails["text_tokens"]; textPresent {
			text := t.count(cachedDetails, "text_tokens", 0)
			if text > t.CachedTokens {
				t.conflict("cached_image_text_partition_mismatch")
			}
			t.CachedImageInputTokens = max(0, t.CachedTokens-text)
			t.fields |= usageFieldCachedImageInput
		} else if text, present := d["text_tokens"]; present && readInt64(text) == 0 && image == total {
			// With a pure-image input the cache partition is uniquely known.
			t.CachedImageInputTokens = t.CachedTokens
			t.fields |= usageFieldCachedImageInput
		} else {
			t.missingDimension("missing_cached_image_input_tokens")
		}
	}
	od := usageDetails(m, outDetails)
	t.ImageOutputTokens = t.count(od, "image_tokens", usageFieldImageOutput)
	if _, present := m[outDetails]; !present && op == config.ProtocolOpenAIImage {
		// The Images protocol defines every output token as an image token.
		// Chat/Responses do not share that contract, even for image models.
		t.ImageOutputTokens = t.OutputTokens
		t.fields |= usageFieldImageOutput
	} else if _, imagePresent := od["image_tokens"]; !imagePresent && op == config.ProtocolOpenAIImage && t.OutputTokens > 0 {
		if _, textPresent := od["text_tokens"]; textPresent {
			text := t.count(od, "text_tokens", 0)
			if text > t.OutputTokens {
				t.conflict("image_output_text_partition_mismatch")
			}
			t.ImageOutputTokens = max(0, t.OutputTokens-text)
			t.fields |= usageFieldImageOutput
		} else {
			t.missingDimension("missing_image_output_tokens")
		}
	}
	if image > total || t.CachedImageInputTokens > image || t.CachedImageInputTokens > t.CachedTokens || image-t.CachedImageInputTokens > t.InputTokens || t.ImageOutputTokens > t.OutputTokens {
		t.conflict("image_partition_exceeds_total")
	}
	// Image and text counts describe the original input total (including cache).
	// Do not guess mixed cached images or redistribute image cache writes.
	if _, present := d["text_tokens"]; present {
		text := t.count(d, "text_tokens", 0)
		if text > total || image != total-text {
			t.conflict("image_text_partition_mismatch")
		}
	}
	if _, present := od["text_tokens"]; present {
		text := t.count(od, "text_tokens", 0)
		if text > t.OutputTokens || t.ImageOutputTokens != t.OutputTokens-text {
			t.conflict("image_output_text_partition_mismatch")
		}
	}
	if _, present := cachedDetails["text_tokens"]; present {
		text := t.count(cachedDetails, "text_tokens", 0)
		if text > t.CachedTokens || t.CachedImageInputTokens != t.CachedTokens-text {
			t.conflict("cached_image_text_partition_mismatch")
		}
	}
	// Clamp cached images before subtracting them from the original image total.
	t.CachedImageInputTokens = max(0, min(t.CachedImageInputTokens, min(image, t.CachedTokens)))
	t.ImageInputTokens = max(0, image-t.CachedImageInputTokens)
	for _, details := range []map[string]any{d, od} {
		if readInt64(details["audio_tokens"]) > 0 {
			t.costIncomplete = true
			if t.diagnostic == "" {
				t.diagnostic = "multimodal_price_dimensions"
			}
		}
	}
}
