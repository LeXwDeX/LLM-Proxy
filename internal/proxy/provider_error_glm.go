package proxy

// usesGLMErrorPatch gates both official GLM business codes and the historical
// HTTP-200 SSE compatibility patch by the explicit target choice.
func usesGLMErrorPatch(target *Target) bool {
	return targetVendor(target) == usageAdapterGLM
}
