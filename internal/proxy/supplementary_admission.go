package proxy

import "github.com/LeXwDeX/LLM-Proxy/internal/config"

// prepareSupplementaryAdmission reads only explicit model identities in the
// final Anthropic request. It never resolves aliases or inspects message content.
// Advisor and explicit fallbacks[].model contracts checked 2026-10-21:
// https://platform.claude.com/docs/en/agents-and-tools/tool-use/advisor-tool
// https://platform.claude.com/docs/en/build-with-claude/refusals-and-fallback
// fallback_model is only the Issue #14 compatibility assumption. The official
// fallbacks:"default" has no explicit identity, so admission cannot check it.
func prepareSupplementaryAdmission(path string, body []byte) ([]byte, []string, []memberNormalization) {
	if requestSchemaForPath(path) != config.RequestSchemaAnthropic {
		return body, nil, nil
	}
	return scanSupplementaryAdmission(body)
}

// scanSupplementaryAdmission normalizes the explicit supplementary members of
// an Anthropic body and lists their model identities. Usage attribution reads
// the same identities from the already-normalized forward body.
func scanSupplementaryAdmission(body []byte) ([]byte, []string, []memberNormalization) {
	members, ok := scanJSONObject(nil, body)
	if !ok {
		return body, nil, nil // Invalid fields/JSON remain the upstream's validation responsibility.
	}
	var edits []byteEdit
	var report []memberNormalization
	normalize := func(object []byte, members []jsonMember, offset int, prefix string, names []string) {
		drop, removed := foldedMemberDrops(members, names)
		if drop == nil {
			return
		}
		for _, edit := range objectMemberDeletions(object, jsonObjectOpen(object), members, drop, nil) {
			edit.start += offset
			edit.end += offset
			edits = append(edits, edit)
		}
		for i, count := range removed {
			if count > 0 {
				report = append(report, memberNormalization{Member: prefix + names[i], Removed: count})
			}
		}
	}
	normalize(body, members, 0, "", []string{"tools", "fallback_model", "fallbacks"})
	var models []string
	addModel := func(raw []byte) {
		if model, ok := jsonString(raw); ok && model != "" {
			models = append(models, model)
		}
	}
	addModel(jsonLast(members, "fallback_model"))
	for _, field := range []string{"tools", "fallbacks"} {
		for i := len(members) - 1; i >= 0; i-- {
			m := members[i]
			if !m.named(field) {
				continue
			}
			if len(m.value) == 0 || m.value[0] != '[' {
				break
			}
			// The outer scanner already validated this array. Visit only its direct
			// object elements, never arbitrary nested model fields.
			s := jsonScanner{data: m.value, pos: 1}
			for s.space(); s.data[s.pos] != ']'; s.space() {
				start := s.pos
				s.value()
				object := s.data[start:s.pos]
				if item, ok := scanJSONObject(nil, object); ok {
					names := []string{"model"}
					checkModel := true
					if field == "tools" {
						names = []string{"type", "name"}
						typ, _ := jsonString(jsonLast(item, "type"))
						name, _ := jsonString(jsonLast(item, "name"))
						if typ == "advisor_20260301" {
							names = append(names, "model")
						}
						checkModel = typ == "advisor_20260301" && name == "advisor"
					}
					if checkModel {
						addModel(jsonLast(item, "model"))
					}
					normalize(object, item, m.valueStart+start, field+"[].", names)
				}
				s.space()
				if s.data[s.pos] == ',' {
					s.pos++
				}
			}
			break // Only the last exact member survives normalization.
		}
	}
	if len(edits) != 0 {
		body = applyByteEdits(body, edits)
	}
	return body, models, report
}
