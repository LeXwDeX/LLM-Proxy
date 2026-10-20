package proxy

import (
	"net/http"
)

// newTargetUsageParser binds accounting to the mapped upstream model and the
// request actually sent after custom headers/body and alias rewriting.
//
// body must be the immutable assembled forward body: it is exactly what the
// outbound request's GetBody replays, so it is used directly instead of
// reading another copy. Pricing fields come from the future started before
// the request was sent (startPricingRequest); without one, or for another
// body, they are computed here as before.
func newTargetUsageParser(r *http.Request, target *Target, model string, body []byte, outbound *http.Request) usageParser {
	p := usageParser{endpointType: targetEndpointType(target), model: model, requestBody: body}
	if r != nil {
		p.protocol = ResolveProtocol(r.URL.Path)
	}
	if target != nil {
		p.endpoint = target.Endpoint
		p.usageAdapter = target.UsageAdapter
	}
	if outbound != nil {
		// The outbound headers already contain the client headers plus
		// credentials and custom headers; clone them once.
		p.requestHeaders = outbound.Header.Clone()
		if fields, ok := pricingRequestFutureFrom(outbound.Context()).join(body); ok {
			p.pricingRequestFields = fields
			p.pricingRequestCaptured = true
		}
	} else {
		if r != nil {
			p.requestHeaders = r.Header.Clone()
		}
		if target != nil {
			if p.requestHeaders == nil {
				p.requestHeaders = make(http.Header)
			}
			for key, value := range target.CustomHeaders {
				p.requestHeaders.Set(key, value)
			}
		}
	}
	return p.capturePricingRequest()
}
