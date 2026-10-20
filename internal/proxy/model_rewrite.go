package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"slices"
	"strings"
)

// replaceModelInBody changes only the top-level JSON model value. It skips only
// an exact spelling match; model-value matching remains case-insensitive. User
// text, metadata, tool arguments and image data keep their original values.
//
// The rewrite is byte-level: only the value bytes of the single top-level
// "model" member change. Other members keep their original bytes, order,
// whitespace and escapes, so large numbers and client formatting survive.
// Invalid JSON and non-object bodies pass through unchanged.
func replaceModelInBody(body []byte, upstream, model string) []byte {
	return rewriteTopLevelJSON(body, topLevelJSONEdits{rewriteModel: true, upstream: upstream, model: model})
}

func replaceModelInRequestBody(body []byte, contentType, upstream, model string) []byte {
	kind, boundary, _ := classifyRequestBody(contentType, body)
	if kind != bodyMultipart {
		return replaceModelInBody(body, upstream, model)
	}
	return replaceModelInMultipartBody(body, boundary, upstream, model)
}

func replaceModelInMultipartBody(body []byte, boundary, upstream, model string) []byte {
	if boundary == "" || model == "" || upstream == model {
		return body
	}
	reader := multipart.NewReader(bytes.NewReader(body), boundary)
	var result bytes.Buffer
	writer := multipart.NewWriter(&result)
	if writer.SetBoundary(boundary) != nil {
		return body
	}
	for {
		part, err := reader.NextRawPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return body
		}
		output, err := writer.CreatePart(part.Header)
		if err != nil {
			return body
		}
		if part.FormName() == "model" && part.FileName() == "" {
			value, err := io.ReadAll(part)
			if err != nil {
				return body
			}
			if strings.EqualFold(strings.TrimSpace(string(value)), model) {
				value = []byte(upstream)
			}
			_, _ = output.Write(value)
		} else if _, err := io.Copy(output, part); err != nil {
			return body
		}
	}
	if writer.Close() != nil {
		return body
	}
	return result.Bytes()
}

// topLevelJSONEdits describes the outbound JSON edits of one attempt. Both
// edits share a single scan of the assembled body.
type topLevelJSONEdits struct {
	rewriteModel    bool
	upstream, model string
	streamUsage     bool
}

type byteEdit struct {
	start, end int
	data       []byte
}

// rewriteTopLevelJSON applies the requested edits with one scan. Request
// entry already normalized the protected members (normalizeProtectedMembers);
// a body that still holds duplicates or case variants of them (a direct
// caller) is normalized here first, so every edit sees the single member the
// proxy uses and upstream readers cannot disagree about it.
func rewriteTopLevelJSON(body []byte, edits topLevelJSONEdits) []byte {
	if edits.rewriteModel && (edits.model == "" || edits.upstream == edits.model) {
		edits.rewriteModel = false
	}
	if (!edits.rewriteModel && !edits.streamUsage) || len(body) == 0 {
		return body
	}
	members, ok := scanJSONObject(nil, body)
	if !ok {
		return body
	}
	if normalized, report := normalizeScannedMembers(body, members); report != nil {
		body = normalized
		if members, ok = scanJSONObject(members[:0], body); !ok {
			return body
		}
	}
	var changes []byteEdit
	if edits.rewriteModel {
		if change, apply := modelValueEdit(members, edits.upstream, edits.model); apply {
			changes = append(changes, change)
		}
	}
	if edits.streamUsage {
		if change, apply := streamUsageEdit(body, members); apply {
			changes = append(changes, change)
		}
	}
	return applyByteEdits(body, changes)
}

// modelValueEdit replaces exactly the value bytes of the top-level "model"
// member (unique after normalization). The match rule is unchanged: a JSON
// string equal to the client model case-insensitively after trimming spaces.
func modelValueEdit(members []jsonMember, upstream, model string) (byteEdit, bool) {
	found := jsonAll(members, "model")
	if len(found) == 0 {
		return byteEdit{}, false
	}
	member := members[found[len(found)-1]]
	var value string
	if json.Unmarshal(member.value, &value) != nil || !strings.EqualFold(strings.TrimSpace(value), model) {
		return byteEdit{}, false
	}
	encoded, err := json.Marshal(upstream)
	if err != nil {
		return byteEdit{}, false
	}
	return byteEdit{start: member.valueStart, end: member.valueEnd, data: encoded}, true
}

func applyByteEdits(body []byte, changes []byteEdit) []byte {
	if len(changes) == 0 {
		return body
	}
	slices.SortStableFunc(changes, func(a, b byteEdit) int { return a.start - b.start })
	size := len(body)
	for _, change := range changes {
		size += len(change.data) - (change.end - change.start)
	}
	out := make([]byte, 0, size)
	position := 0
	for _, change := range changes {
		out = append(out, body[position:change.start]...)
		out = append(out, change.data...)
		position = change.end
	}
	return append(out, body[position:]...)
}
