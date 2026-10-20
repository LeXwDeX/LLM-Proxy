package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"math"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

// pricingValue retains exact spelling. Only protocol-defined values normalize;
// null, empty and unknown values cannot establish an actual billing tier.
func pricingValue(m map[string]any, key string, allowed map[string]string) (usage.PricingValue, string) {
	value, present := m[key]
	if !present {
		return usage.PricingValue{State: usage.PricingValueMissing}, ""
	}
	if value == nil {
		return usage.PricingValue{State: usage.PricingValueNull}, ""
	}
	raw, ok := value.(string)
	if !ok {
		encoded, _ := json.Marshal(value)
		return usage.PricingValue{Raw: string(encoded), State: usage.PricingValueUnknown}, ""
	}
	if raw == "" {
		return usage.PricingValue{State: usage.PricingValueEmpty}, ""
	}
	canonical, ok := allowed[raw]
	if !ok {
		return usage.PricingValue{Raw: raw, State: usage.PricingValueUnknown}, ""
	}
	return usage.PricingValue{Raw: raw, State: usage.PricingValueValue}, canonical
}

var openAIServiceTiers = map[string]string{"default": "standard", "fast": "fast", "priority": "fast", "ultrafast": "ultrafast", "flex": "flex", "auto": ""}
var claudeServiceTiers = map[string]string{"standard": "standard", "priority": "priority", "auto": ""}
var claudeSpeeds = map[string]string{"standard": "standard", "fast": "fast"}

// Only protocol envelope locations are examined, never generated content.
func pricingEnvelope(payload map[string]any, op config.ProtocolType) map[string]any {
	var keys []string
	switch op {
	case config.ProtocolOpenAIResponses:
		keys = []string{"response"}
	case config.ProtocolAnthropicMessages:
		keys = []string{"message"}
	}
	for _, key := range keys {
		if nested, ok := payload[key].(map[string]any); ok {
			return nested
		}
	}
	return payload
}

func (p usageParser) pricingContext(payload, m map[string]any) *usage.PricingContext {
	op := p.operation(m)
	if op == config.ProtocolGemini {
		return nil
	}
	c := &usage.PricingContext{Version: usage.PricingContextVersion}
	request := p.pricingRequestFields
	tiers := openAIServiceTiers
	if op == config.ProtocolAnthropicMessages {
		tiers = claudeServiceTiers
	}
	c.RequestServiceTier, _ = pricingValue(request, "service_tier", tiers)
	c.RequestSpeed = usage.PricingValue{State: usage.PricingValueMissing}
	if op == config.ProtocolAnthropicMessages {
		c.RequestSpeed, _ = pricingValue(request, "speed", claudeSpeeds)
	}
	envelope := pricingEnvelope(payload, op)
	response := envelope
	if op == config.ProtocolAnthropicMessages {
		response = m
	}
	c.ResponseServiceTier, c.ActualServiceTier = pricingValue(response, "service_tier", tiers)
	c.ResponseSpeed = usage.PricingValue{State: usage.PricingValueMissing}
	if op == config.ProtocolAnthropicMessages {
		c.ResponseSpeed, c.ActualSpeed = pricingValue(m, "speed", claudeSpeeds)
	}
	var observed usageTokens
	keys := []string{"input_tokens", "cache_read_input_tokens", "cache_creation_input_tokens"}
	if op != config.ProtocolAnthropicMessages {
		input, _, _, _ := openAIUsageKeys(m, op)
		keys = []string{input}
	}
	valid := true
	for _, key := range keys {
		_, present := m[key]
		c.TotalInputKnown = c.TotalInputKnown || present
		valid = valid && present
		n := observed.count(m, key, 0)
		if observed.status == usageStatusConflict {
			valid = false
		}
		if n > math.MaxInt64-c.TotalInputTokens {
			valid = false
			c.TotalInputTokens = 0
			c.Diagnostic = "total_input_overflow"
			break
		}
		c.TotalInputTokens += n
	}
	c.TotalInputValid = valid
	if !valid && c.Diagnostic == "" {
		c.Diagnostic = "missing_or_invalid_total_input"
	}
	return c
}

// Stream snapshots merge only explicit envelope values. Omission keeps earlier
// evidence, while null/empty/unknown terminal values replace it.
func (u *usageStream) observePricingEnvelope(payload map[string]any) {
	envelope := pricingEnvelope(payload, u.parser.operation(u.snapshot))
	if u.pricingEnvelope == nil {
		u.pricingEnvelope = map[string]any{}
	}
	if value, present := envelope["service_tier"]; present {
		u.pricingEnvelope["service_tier"] = value
	}
}

func (u *usageStream) updatePricingContext() {
	payload := map[string]any{"usage": u.snapshot, "model": u.model}
	for key, value := range u.pricingEnvelope {
		payload[key] = value
	}
	u.tokens.pricingContext = u.parser.pricingContext(payload, u.snapshot)
}

// Project request metadata without retaining prompts, images or generated data.
func pricingRequest(body []byte) map[string]any {
	request, _ := pricingRequestUntil(nil, body)
	return request
}

// pricingRequestUntil is pricingRequest that stops early once done is closed,
// checked between top-level members. complete=false means the projection was
// abandoned and its result must not be used; otherwise it equals pricingRequest.
func pricingRequestUntil(done <-chan struct{}, body []byte) (request map[string]any, complete bool) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, true
	}
	request = map[string]any{}
	for decoder.More() {
		if done != nil {
			select {
			case <-done:
				return nil, false
			default:
			}
		}
		token, err = decoder.Token()
		if err != nil {
			return nil, true
		}
		key, _ := token.(string)
		if key == "service_tier" || key == "speed" {
			var value any
			if decoder.Decode(&value) != nil {
				return nil, true
			}
			request[key] = value
		} else if skipUsageValue(decoder) != nil {
			return nil, true
		}
	}
	if _, err = decoder.Token(); err != nil {
		return nil, true
	}
	return request, true
}

// pricingRequestFuture projects the outbound body's pricing fields on its own
// goroutine while the request is sent and response headers are pending, off
// the TTFT path. The body slice is immutable once assembled. The goroutine
// does bounded CPU work, never blocks and stops early when the attempt
// context ends, so it cannot leak; nobody has to join it.
type pricingRequestFuture struct {
	body     []byte
	done     chan struct{}
	fields   map[string]any
	complete bool
}

type pricingRequestFutureKey struct{}

// startPricingRequest returns ctx carrying a future for body. Empty bodies
// need no goroutine: their projection is trivially nil.
func startPricingRequest(ctx context.Context, body []byte) context.Context {
	if len(body) == 0 {
		return ctx
	}
	future := &pricingRequestFuture{body: body, done: make(chan struct{})}
	go func() {
		defer close(future.done)
		future.fields, future.complete = pricingRequestUntil(ctx.Done(), body)
	}()
	return context.WithValue(ctx, pricingRequestFutureKey{}, future)
}

func pricingRequestFutureFrom(ctx context.Context) *pricingRequestFuture {
	if ctx == nil {
		return nil
	}
	future, _ := ctx.Value(pricingRequestFutureKey{}).(*pricingRequestFuture)
	return future
}

// join waits for the projection of exactly body (same backing array and
// length). A different body or an abandoned projection reports false, and the
// caller computes synchronously, so results never differ from pricingRequest.
func (f *pricingRequestFuture) join(body []byte) (map[string]any, bool) {
	if f == nil || len(body) == 0 || len(body) != len(f.body) || &body[0] != &f.body[0] {
		return nil, false
	}
	<-f.done
	return f.fields, f.complete
}

// Capture request evidence once before consuming response events. Parser copies
// share this private immutable projection, not the potentially large body.
func (p usageParser) capturePricingRequest() usageParser {
	if !p.pricingRequestCaptured {
		p.pricingRequestFields = pricingRequest(p.requestBody)
		p.pricingRequestCaptured = true
	}
	return p
}
