package proxy

// requestStreamUsageIfMissing preserves explicit values (including malformed
// ones). Only "include_usage":true is inserted; every other byte of the body,
// including unrelated large numbers and nested members, stays intact.
func requestStreamUsageIfMissing(body []byte) []byte {
	return rewriteTopLevelJSON(body, topLevelJSONEdits{streamUsage: true})
}

// streamUsageEdit computes the byte insertion for the opt-in policy, on a
// body whose protected members are already unique. Only a literal top-level
// stream true qualifies, a present stream_options must be an object, and an
// existing include_usage of any type is kept.
func streamUsageEdit(body []byte, members []jsonMember) (byteEdit, bool) {
	if string(jsonLast(members, "stream")) != "true" {
		return byteEdit{}, false
	}
	found := jsonAll(members, "stream_options")
	if len(found) == 0 {
		last := members[len(members)-1]
		return byteEdit{start: last.valueEnd, end: last.valueEnd, data: []byte(`,"stream_options":{"include_usage":true}`)}, true
	}
	value := members[found[len(found)-1]]
	if value.value[0] != '{' {
		// null, arrays and scalars are explicit operator/client choices.
		return byteEdit{}, false
	}
	inner, ok := scanJSONObject(nil, value.value)
	if !ok {
		return byteEdit{}, false
	}
	if len(inner) == 0 {
		at := value.valueStart + 1
		return byteEdit{start: at, end: at, data: []byte(`"include_usage":true`)}, true
	}
	if jsonHas(inner, "include_usage") {
		return byteEdit{}, false
	}
	at := value.valueStart + inner[len(inner)-1].valueEnd
	return byteEdit{start: at, end: at, data: []byte(`,"include_usage":true`)}, true
}
