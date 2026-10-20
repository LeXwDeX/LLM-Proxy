package proxy

import (
	"net/http"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

// sanitizeTargetBody runs the provider policy before operator custom fields.
// Callers retain control of caching and logging the stripped fields.
func sanitizeTargetBody(profile *ProviderProfile, r *http.Request, body []byte) ([]byte, []string) {
	if profile != nil && profile.Body.SanitizeFunc != nil {
		return profile.Body.SanitizeFunc(r, body)
	}
	return body, nil
}

// prepareTargetBody finishes assembly after sanitize/multipart conversion.
// Each attempt must start with the client's original model and URL path.
func prepareTargetBody(target *Target, r *http.Request, body []byte, requestedModel, upstreamModel string) []byte {
	body = injectCustomBody(body, target.CustomBody)
	// Unrestricted targets retain the original client spelling. The normalized
	// upstreamModel remains the stable identity for quota, affinity and usage.
	wireModel := upstreamModel
	if target.allowedModelIdx == nil {
		wireModel = requestedModel
	}
	if wireModel != "" {
		wireModel = target.ModelPrefix + wireModel
	}
	// JSON edits share one index scan and change only the edited bytes: the
	// top-level model value and the inserted include_usage member.
	var edits topLevelJSONEdits
	if wireModel != "" && wireModel != requestedModel {
		r.URL.Path = replaceModelInURLPath(r.URL.Path, wireModel, requestedModel)
		r.URL.RawPath = "" // URL.String safely escapes ?, # and % in model names.
		if kind, boundary, _ := classifyRequestBody(r.Header.Get("Content-Type"), body); kind == bodyMultipart {
			body = replaceModelInMultipartBody(body, boundary, wireModel, requestedModel)
		} else {
			edits.rewriteModel, edits.upstream, edits.model = true, wireModel, requestedModel
		}
	}
	// Operator top-level replacement comes first. The opt-in policy only fills
	// fields still absent in this attempt's assembled body; it never repairs types.
	if target.RequestStreamUsageIfMissing && requestSchemaForPath(r.URL.Path) == config.StreamUsageRequestSchema {
		edits.streamUsage = true
	}
	return rewriteTopLevelJSON(body, edits)
}

// injectTargetRequestHeaders protects provider credentials and routing headers
// with the same runtime blacklist in normal forwarding and model probes.
func injectTargetRequestHeaders(req *http.Request, target *Target, auth AuthStrategy, apiKey, clientPath string) error {
	if auth != nil {
		if err := auth.InjectAuth(req, apiKey, clientPath); err != nil {
			return err
		}
	}
	injectCustomHeaders(req, target.CustomHeaders)
	return nil
}
