package proxy

import (
	"math"
	"strings"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

// Claude input_tokens excludes both cache partitions. TTL counters partition W.
// modelPrefix is the target's ModelPrefix; see stripModelPrefix.
func parseClaudeUsage(m map[string]any, modelPrefix string) usageTokens {
	t := parseClaudeUsageCounters(m)
	if iterations, ok := m["iterations"].([]any); ok && len(iterations) > 0 {
		if reason := claudeIterationsIncomplete(iterations, m, modelPrefix); reason != "" {
			t.costIncomplete = true
			t.diagnostic = reason
		}
	}
	if claudeServerToolCharges(usageDetails(m, "server_tool_use")) {
		t.costIncomplete = true
		t.diagnostic = "server_tool_charges"
	}
	return t
}

func parseClaudeUsageCounters(m map[string]any) usageTokens {
	var t usageTokens
	t.InputTokens = t.count(m, "input_tokens", usageFieldInput)
	t.OutputTokens = t.count(m, "output_tokens", usageFieldOutput)
	// Thinking is already included in output; preserve it for observation only.
	t.ReasoningTokens = t.count(usageDetails(m, "output_tokens_details"), "thinking_tokens", usageFieldReasoning)
	if t.ReasoningTokens > t.OutputTokens {
		t.conflict("reasoning_exceeds_output")
	}
	t.CachedTokens = t.count(m, "cache_read_input_tokens", usageFieldCached)
	t.CacheCreationTokens = t.count(m, "cache_creation_input_tokens", usageFieldCacheCreation)
	d := usageDetails(m, "cache_creation")
	t.CacheCreation5mTokens = t.count(d, "ephemeral_5m_input_tokens", usageFieldCacheCreation5m)
	t.CacheCreation1hTokens = t.count(d, "ephemeral_1h_input_tokens", usageFieldCacheCreation1h)
	if t.CacheCreation5mTokens > t.CacheCreationTokens || t.CacheCreation1hTokens > t.CacheCreationTokens-t.CacheCreation5mTokens {
		t.conflict("cache_ttl_exceeds_creation")
	}
	return t
}

// Only known, valid zero counters establish no tool charge. Unknown members or
// invalid counters keep billing conservative without inventing token usage.
func claudeServerToolCharges(tools map[string]any) bool {
	for key := range tools {
		switch key {
		case "web_fetch_requests", "web_search_requests":
			var observed usageTokens
			if observed.count(tools, key, 0) > 0 || observed.status == usageStatusConflict {
				return true
			}
		default:
			return true
		}
	}
	return false
}

// claudeIterationCounters are the top-level counters each executor iteration
// may repeat. Top-level usage is the sum of all type "message" iterations.
var claudeIterationCounters = [...]string{"input_tokens", "output_tokens", "cache_read_input_tokens", "cache_creation_input_tokens"}

// claudeIterationsIncomplete decides whether top-level usage alone prices a
// response whose usage carries a non-empty iterations array. It never adds
// iteration counters to the billed tokens; it only names why the cost may be
// incomplete, or returns "" when top-level usage covers every iteration.
//
// Official contracts checked 2026-10-21 (a documentation check is not
// acceptance against the real API):
//   - advisor tool: "message" items are executor sampling billed at the
//     executor price and every top-level field is their sum; "advisor_message"
//     items carry their own model and price and are excluded from top-level
//     https://platform.claude.com/docs/en/agents-and-tools/tool-use/advisor-tool
//   - threshold compaction: "compaction" items are excluded from top-level;
//     with the beta every response carries iterations, items may omit cache
//     counters https://platform.claude.com/docs/en/build-with-claude/compaction-threshold
//   - on-demand compaction: top-level is zero, usage only in the compaction item
//     https://platform.claude.com/docs/en/build-with-claude/compaction-on-demand
//   - server-side fallback: "fallback_message" items are billed at the
//     fallback model price; top-level covers only the returned attempt
//     https://platform.claude.com/docs/en/build-with-claude/refusals-and-fallback
//
// Priority: multi_model_iterations > compaction_iterations >
// unknown_usage_iterations > iterations_usage_mismatch. Items without a type,
// with an unknown type, invalid or missing required counters, a non-string or
// empty model, or non-object items have unknown
// attribution and stay conservative. Applies to every target parsed by the
// Anthropic rule; it never infers anything from host or model name. Echoed
// models are compared after removing the target's own ModelPrefix (issue #36).
func claudeIterationsIncomplete(iterations []any, top map[string]any, modelPrefix string) string {
	var multiModel, compaction, unknown, mismatch bool
	model := ""
	var sums [len(claudeIterationCounters)]int64
	var reported [len(claudeIterationCounters)]int
	messages := 0
	for _, raw := range iterations {
		item, ok := raw.(map[string]any)
		if !ok {
			unknown = true
			continue
		}
		var observed usageTokens
		var counts [len(claudeIterationCounters)]int64
		for i, key := range claudeIterationCounters {
			counts[i] = observed.count(item, key, 0)
		}
		if observed.status == usageStatusConflict {
			unknown = true
		}
		if v, present := item["model"]; present {
			// A non-string or empty model is unknown attribution, not "no
			// model seen yet".
			name, ok := v.(string)
			switch {
			case !ok || name == "":
				unknown = true
			case model == "":
				model = stripModelPrefix(name, modelPrefix)
			case stripModelPrefix(name, modelPrefix) != model:
				multiModel = true
			}
		}
		typ, _ := item["type"].(string)
		switch typ {
		case "message":
			messages++
			for i, key := range claudeIterationCounters {
				if _, present := item[key]; !present {
					if key == "input_tokens" || key == "output_tokens" {
						unknown = true
					}
					continue
				}
				reported[i]++
				if counts[i] > math.MaxInt64-sums[i] {
					mismatch = true
				}
				sums[i] += counts[i]
			}
		case "advisor_message", "fallback_message":
			multiModel = true
		case "compaction":
			compaction = true
		default:
			unknown = true
		}
	}
	// A complete sum must equal top-level. A partial sum is only a lower
	// bound, and can disprove consistency only against a reported top-level
	// count; missing optional counters must not be inferred as zero.
	for i, key := range claudeIterationCounters {
		if messages == 0 || reported[i] == 0 {
			continue
		}
		if key == "cache_read_input_tokens" || key == "cache_creation_input_tokens" {
			if _, present := top[key]; !present {
				// Known positive cache usage cannot be priced from a missing
				// top-level counter, even if other iterations omit it too.
				if sums[i] > 0 {
					unknown = true
				}
				continue
			}
		}
		var observed usageTokens
		topCount := observed.count(top, key, 0)
		if reported[i] == messages {
			if topCount != sums[i] {
				mismatch = true
			}
		} else if _, present := top[key]; present && sums[i] > topCount {
			mismatch = true
		}
	}
	switch {
	case multiModel:
		return "multi_model_iterations"
	case compaction:
		return "compaction_iterations"
	case unknown:
		return "unknown_usage_iterations"
	case mismatch:
		return "iterations_usage_mismatch"
	}
	return ""
}

// stripModelPrefix removes the target's ModelPrefix (added only to the outbound
// model) from a model name the upstream echoed. Only a complete,
// case-insensitive prefix match that leaves a non-empty name is removed; any
// other spelling difference (snapshot IDs, aliases) is never inferred.
func stripModelPrefix(name, prefix string) string {
	if prefix == "" || len(name) <= len(prefix) || !strings.EqualFold(name[:len(prefix)], prefix) {
		return name
	}
	return name[len(prefix):]
}

// echoedModel is the lowercased attribution identity of an echoed model name.
func (p usageParser) echoedModel(name string) string {
	return strings.ToLower(stripModelPrefix(name, p.modelPrefix))
}

// claudeEchoMismatch names an echoed explicit model outside the request's
// admission set. It is only logged; attribution keeps the echoed spelling.
type claudeEchoMismatch struct{ iterationType, model string }

// claudeAdmission is the set of model identities the request admission
// checked on this target: the executor (upstream model) plus the explicit
// advisor/fallback models of the final outbound body.
func (p usageParser) claudeAdmission() []string {
	_, models, _ := scanSupplementaryAdmission(p.requestBody)
	return append([]string{p.model}, models...)
}

// admit returns the admission-side spelling for an echoed explicit model that
// matches it case-insensitively (lowercased, like every usage model identity).
// Otherwise the echoed spelling is kept and the mismatch recorded.
func (t *usageTokens) admit(admission []string, echoed, typ string) string {
	for _, model := range admission {
		if model = strings.TrimSpace(model); model != "" && strings.EqualFold(model, echoed) {
			return strings.ToLower(model)
		}
	}
	t.unadmittedEchoes = append(t.unadmittedEchoes, claudeEchoMismatch{iterationType: typ, model: echoed})
	return echoed
}

// parseClaudeIterationUsage expands only an entirely validated attribution plan.
// The root's counters remain the top-level usage. Executor messages overlap that
// root; compaction/advisor do not. Fallback instead overlaps only the last attempt.
// Any unknown shape retains the issue #7 diagnostic and adds no guessed charges.
func (p usageParser) parseClaudeIterationUsage(top map[string]any, responseModel string) usageTokens {
	legacy := parseClaudeUsage(top, p.modelPrefix)
	// invalid: the attribution plan is unverifiable, so it must not replace
	// committed evidence. Legitimate cost_incomplete reasons are not invalid.
	invalid := func() usageTokens {
		legacy.invalidIterations = true
		return legacy
	}
	raw, ok := top["iterations"].([]any)
	if !ok {
		if _, present := top["iterations"]; present {
			// null, objects and scalars cannot prove a complete plan.
			return invalid()
		}
		return legacy
	}
	if len(raw) == 0 {
		return legacy
	}
	base := parseClaudeUsageCounters(top)
	if !validClaudeIterationCounters(top, base) {
		if base.status != usageStatusConflict && base.fields&(usageFieldInput|usageFieldOutput) == usageFieldInput|usageFieldOutput {
			// Valid counters whose cost context overflows.
			return invalid()
		}
		// Missing input cannot prove the full overlap plan. The documented last
		// fallback still identifies whose observed output it is; never copy input
		// from that iteration or price it with the executor's request context.
		final, _ := raw[len(raw)-1].(map[string]any)
		model := p.echoedModel(readString(final["model"]))
		finalTokens := parseClaudeUsageCounters(final)
		if _, input := top["input_tokens"]; !input && base.status != usageStatusConflict &&
			base.fields&usageFieldOutput != 0 && final["type"] == "fallback_message" && model != "" &&
			validClaudeIterationCounters(final, finalTokens) && finalTokens.OutputTokens == base.OutputTokens {
			model = legacy.admit(p.claudeAdmission(), model, "fallback_message")
			legacy.billingModel, legacy.iterationType, legacy.iterationIndex = model, "fallback_message", len(raw)-1
			independent := usageParser{protocol: config.ProtocolAnthropicMessages}
			legacy.pricingContext = independent.pricingContext(nil, top)
			legacy.costIncomplete = true
			appendTokenDiagnostic(&legacy, "missing_fallback_input_usage")
		}
		return legacy
	}
	// The request-side upstream model never carries ModelPrefix; the response
	// envelope fallback is an echo and is compared without it.
	executor := strings.ToLower(strings.TrimSpace(p.model))
	if executor == "" {
		executor = p.echoedModel(strings.TrimSpace(responseModel))
	}
	items := make([]map[string]any, len(raw))
	models := make([]string, len(raw))
	explicit := make([]bool, len(raw))
	messages := []any{}
	hasFallback, hasAdditional := false, false
	messagesMatchExecutor := true
	for i, value := range raw {
		item, ok := value.(map[string]any)
		if !ok {
			return invalid()
		}
		tokens := parseClaudeUsageCounters(item)
		if !validClaudeIterationCounters(item, tokens) {
			return invalid()
		}
		if _, nested := item["iterations"]; nested {
			return invalid()
		}
		model := executor
		if value, present := item["model"]; present {
			name, ok := value.(string)
			if !ok || strings.TrimSpace(name) == "" {
				return invalid()
			}
			model, explicit[i] = p.echoedModel(strings.TrimSpace(name)), true
		}
		typ, _ := item["type"].(string)
		switch typ {
		case "message":
			if executor != "" && model != executor {
				messagesMatchExecutor = false
			}
			messages = append(messages, item)
		case "compaction":
			if model == "" {
				return invalid()
			}
			hasAdditional = true
		case "advisor_message", "fallback_message":
			if _, present := item["model"]; !present || model == "" {
				return invalid()
			}
			hasAdditional = true
			hasFallback = hasFallback || typ == "fallback_message"
		default:
			return invalid()
		}
		items[i], models[i] = item, model
	}
	// Keep the existing top-only diagnostics, but only a verified plan may
	// replace committed evidence.
	if !hasAdditional {
		if !messagesMatchExecutor || claudeIterationsIncomplete(messages, top, p.modelPrefix) != "" {
			return invalid()
		}
		return legacy
	}
	returned := -1
	if hasFallback {
		returned = len(items) - 1
		if items[returned]["type"] != "fallback_message" {
			return invalid()
		}
		for _, item := range items {
			if item["type"] != "message" && item["type"] != "fallback_message" {
				return invalid()
			}
		}
		final := make(map[string]any, len(items[returned]))
		for k, v := range items[returned] {
			final[k] = v
		}
		final["type"] = "message"
		if claudeIterationsIncomplete([]any{final}, top, p.modelPrefix) != "" {
			return invalid()
		}
	} else {
		if !messagesMatchExecutor {
			return invalid()
		}
		if len(messages) == 0 {
			// The on-demand compaction contract is the only zero-executor shape.
			for _, item := range items {
				if item["type"] != "compaction" {
					return invalid()
				}
			}
			if base.InputTokens != 0 || base.OutputTokens != 0 || base.CachedTokens != 0 || base.CacheCreationTokens != 0 {
				return invalid()
			}
		} else if claudeIterationsIncomplete(messages, top, p.modelPrefix) != "" {
			return invalid()
		}
	}
	totals := []int64{base.InputTokens, base.OutputTokens, base.CachedTokens, base.CacheCreationTokens, base.ReasoningTokens, base.CacheCreation5mTokens, base.CacheCreation1hTokens}
	// Bill each explicit echo under the identity admission checked, so quota
	// consumption lands on the same model group (issue #36).
	admission := p.claudeAdmission()
	for i, item := range items {
		typ := item["type"].(string)
		if !hasFallback && typ == "message" {
			continue
		}
		if explicit[i] && models[i] != "" {
			models[i] = base.admit(admission, models[i], typ)
		}
		if i == returned {
			base.billingModel, base.iterationType, base.iterationIndex = models[i], typ, i
			// The actual returned attempt has no evidence about its own request tier.
			independent := usageParser{protocol: config.ProtocolAnthropicMessages}
			base.pricingContext = independent.pricingContext(nil, top)
			continue
		}
		if models[i] == "" {
			return invalid()
		}
		component := p.claudeIterationComponent(item, models[i], typ, i)
		if hasFallback && component.OutputTokens == 0 {
			// The public fallback example omits prior-attempt categories. Final
			// stop_details cannot classify a different attempt, so retain its tokens
			// but defer the uncertain charge instead of declaring it truly free.
			component.NonBillable, component.CostIncomplete = true, true
			component.UsageDiagnostic = "unknown_fallback_refusal_billing"
			base.costIncomplete, base.diagnostic = true, component.UsageDiagnostic
		}
		for k, n := range []int64{component.InputTokens, component.OutputTokens, component.CachedTokens, component.CacheCreationTokens, component.ReasoningTokens, component.CacheCreation5mTokens, component.CacheCreation1hTokens} {
			if n > math.MaxInt64-totals[k] {
				return invalid()
			}
			totals[k] += n
		}
		base.components = append(base.components, component)
	}
	if claudeServerToolCharges(usageDetails(top, "server_tool_use")) {
		base.costIncomplete, base.diagnostic = true, "server_tool_charges"
	}
	return base
}

func validClaudeIterationCounters(item map[string]any, tokens usageTokens) bool {
	if tokens.status == usageStatusConflict || tokens.fields&(usageFieldInput|usageFieldOutput) != usageFieldInput|usageFieldOutput {
		return false
	}
	for _, key := range []string{"cache_creation", "output_tokens_details", "server_tool_use"} {
		if value, present := item[key]; present {
			if _, ok := value.(map[string]any); !ok {
				return false
			}
		}
	}
	// Cost context sums all input partitions; an overflow cannot be priced.
	return tokens.InputTokens <= math.MaxInt64-tokens.CachedTokens && tokens.CacheCreationTokens <= math.MaxInt64-tokens.InputTokens-tokens.CachedTokens
}

func (p usageParser) claudeIterationComponent(item map[string]any, model, typ string, index int) usage.Component {
	// No executor request fields, service tier, speed, or threshold are copied.
	independent := usageParser{protocol: config.ProtocolAnthropicMessages, model: model, usageAdapter: p.usageAdapter, pricingRequestCaptured: true}
	tokens, _ := independent.parseMap(item, model)
	return usage.Component{
		Model: model, IterationType: typ, IterationIndex: index,
		PricingContext: independent.pricingContext(nil, item), UsageProtocol: string(config.ProtocolAnthropicMessages), ResponseModel: model,
		ObservedFields: uint16(tokens.fields), UsageStatus: tokens.status, UsageAdapter: tokens.adapter,
		CostIncomplete: tokens.costIncomplete, UsageDiagnostic: tokens.diagnostic,
		InputTokens: tokens.InputTokens, OutputTokens: tokens.OutputTokens, CachedTokens: tokens.CachedTokens,
		CacheCreationTokens: tokens.CacheCreationTokens, ExplicitCacheReadTokens: tokens.ExplicitCacheReadTokens,
		ReasoningTokens: tokens.ReasoningTokens, CacheCreation5mTokens: tokens.CacheCreation5mTokens, CacheCreation1hTokens: tokens.CacheCreation1hTokens,
	}
}

// Refusal billing is official structured metadata, never explanation text.
// Docs checked 2026-10-21: https://platform.claude.com/docs/en/build-with-claude/refusals-and-fallback
// All refusals, not only fallback: bio/frontier_llm/reasoning_extraction bill
// before output; "any other category, or ... null" is free. Issue #12 deliberately
// defers unknown categories with a diagnostic because the billed list may change;
// this is a local conservative policy, not a restriction in the official rule.
func (p usageParser) finalizeClaudeRefusalBilling(t *usageTokens, payload map[string]any) {
	if t.OutputTokens != 0 || t.fields&usageFieldOutput == 0 || t.status == usageStatusConflict {
		return
	}
	envelope := pricingEnvelope(payload, config.ProtocolAnthropicMessages)
	if delta, ok := payload["delta"].(map[string]any); ok {
		envelope = delta
	}
	if readString(envelope["stop_reason"]) != "refusal" {
		return
	}
	t.nonBillable = true
	details, ok := envelope["stop_details"].(map[string]any)
	if ok {
		category, present := details["category"]
		if present && category == nil {
			return
		}
		if name, valid := category.(string); present && valid {
			switch name {
			case "bio", "frontier_llm", "reasoning_extraction":
				t.nonBillable = false
				return
			case "cyber", "general_harms":
				return
			}
		}
	}
	t.costIncomplete = true
	if t.diagnostic == "" {
		t.diagnostic = "unknown_refusal_billing"
		if t.billingModel != "" {
			t.diagnostic = "unknown_fallback_refusal_billing"
		}
	}
}
