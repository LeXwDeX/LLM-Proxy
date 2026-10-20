// multipart_convert.go — multipart/form-data → JSON 自动转换。
//
// 当上游 target 只接受 application/json（如部分 API 网关），但客户端发送了
// multipart/form-data（如 OpenAI /images/edits），代理层自动将 multipart body
// 转换为等效的 JSON body，使请求能透明通过。
//
// 文件字段转为 base64 内联：[{type: "base64", media_type: "<mime>", data: "<b64>"}]
// 文本字段保持原样，数值字段（如 "n"）自动转为数字类型。
package proxy

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
)

// imageEndpointSuffixes 列出需要 multipart→JSON 转换的 API 端点后缀。
var imageEndpointSuffixes = []string{
	"/images/edits",
	"/images/variations",
}

// numericFields 列出应当转为数字类型的 multipart 文本字段。
var numericFields = map[string]struct{}{
	"n": {},
}

// needsMultipartConvert 判断请求是否需要 multipart→JSON 转换。
// 条件：Content-Type 是 multipart/form-data 且路径匹配图片编辑端点。
func needsMultipartConvert(r *http.Request) bool {
	if r == nil {
		return false
	}
	// classifyRequestBody decides multipart (the body does not matter for it).
	if kind, _, _ := classifyRequestBody(r.Header.Get("Content-Type"), nil); kind != bodyMultipart {
		return false
	}
	pathLower := strings.ToLower(r.URL.Path)
	for _, suffix := range imageEndpointSuffixes {
		if strings.HasSuffix(pathLower, suffix) {
			return true
		}
	}
	return false
}

// convertMultipartToJSON 将 multipart/form-data body 转换为等效的 JSON body。
//
// 返回值：
//   - jsonBody: 转换后的 JSON bytes
//   - contentType: "application/json"
//   - err: 解析或转换错误
//
// 转换规则：
//   - 有 filename 的 part（文件字段）→ data URI 字符串 "data:<mime>;base64,<b64>"
//   - 同名文件字段多次出现 → 聚合为 data URI 字符串数组
//   - 单文件字段 → 直接输出字符串（而非数组）
//   - 无 filename 的 part（文本字段）→ 字符串值（numericFields 转为数字）
//
// 该格式与 OpenAI /images/edits API 的 image 参数兼容（String | Array[String]）。
func convertMultipartToJSON(r *http.Request, body []byte) ([]byte, string, error) {
	if r == nil || len(body) == 0 {
		return nil, "", errors.New("empty request or body")
	}

	// The raw boundary every other multipart reader uses.
	_, boundary, err := classifyRequestBody(r.Header.Get("Content-Type"), body)
	if err != nil {
		return nil, "", errors.New("invalid Content-Type: " + err.Error())
	}
	if boundary == "" {
		return nil, "", errors.New("missing multipart boundary")
	}

	// 解析结果：文本字段和文件字段分开收集
	textFields := make(map[string]string)
	fileFields := make(map[string][]string) // data URI 字符串

	reader := multipart.NewReader(bytes.NewReader(body), boundary)
	for {
		part, partErr := reader.NextPart()
		if errors.Is(partErr, io.EOF) {
			break
		}
		if partErr != nil {
			return nil, "", errors.New("multipart parse error: " + partErr.Error())
		}

		fieldName := part.FormName()
		fileName := part.FileName()

		if fileName != "" {
			// 文件字段 → 读取内容并转为 data URI
			data, readErr := io.ReadAll(part)
			_ = part.Close()
			if readErr != nil {
				return nil, "", errors.New("read file part error: " + readErr.Error())
			}

			// 确定 MIME 类型
			partCT := part.Header.Get("Content-Type")
			if partCT == "" {
				partCT = detectMimeType(fileName)
			}

			dataURI := "data:" + partCT + ";base64," + base64.StdEncoding.EncodeToString(data)
			fileFields[fieldName] = append(fileFields[fieldName], dataURI)
		} else {
			// 文本字段 → 读取值
			data, readErr := io.ReadAll(io.LimitReader(part, 64*1024)) // 文本字段限 64KB
			_ = part.Close()
			if readErr != nil {
				return nil, "", errors.New("read text part error: " + readErr.Error())
			}
			textFields[fieldName] = strings.TrimSpace(string(data))
		}
	}

	// 组装 JSON payload
	payload := make(map[string]any, len(textFields)+len(fileFields))

	for k, v := range textFields {
		if _, isNumeric := numericFields[k]; isNumeric {
			if n, parseErr := strconv.Atoi(v); parseErr == nil {
				payload[k] = n
				continue
			}
		}
		payload[k] = v
	}

	for k, entries := range fileFields {
		if len(entries) == 1 {
			// 单文件 → 字符串（兼容 OpenAI image: String 格式）
			payload[k] = entries[0]
		} else {
			// 多文件 → 字符串数组（兼容 OpenAI image: Array[String] 格式）
			payload[k] = entries
		}
	}

	jsonBody, marshalErr := json.Marshal(payload)
	if marshalErr != nil {
		return nil, "", errors.New("json marshal error: " + marshalErr.Error())
	}

	return jsonBody, "application/json", nil
}

// --- protected form field normalization ---

// protectedFormFields are the multipart field names normalized like the JSON
// protected members: every JSON protected member plus the image and audio
// form fields that select output, size or quality and therefore cost.
var protectedFormFields = append(append([]string{}, protectedRequestMembers...),
	"response_format", "size", "quality", "output_format", "output_compression", "partial_images", "input_fidelity", "moderation")

func protectedFormFieldIndex(name string) int {
	for i, field := range protectedFormFields {
		if strings.EqualFold(name, field) {
			return i
		}
	}
	return -1
}

// normalizeMultipartMembers applies the JSON normalization rule to form
// fields. The proxy uses the last part whose field name equals a protected
// name exactly (extractMultipartModel, and convertMultipartToJSON where the
// last field wins); every other part whose name folds to that name, exact
// duplicates and case variants alike, is removed. With only variants, all are
// removed. Kept parts retain their bytes, order and the boundary; a body
// needing no change is returned as is. Bodies that do not parse are unchanged.
func normalizeMultipartMembers(body []byte, boundary string) ([]byte, []memberNormalization) {
	if len(body) == 0 || boundary == "" {
		return body, nil
	}
	// Parse strictly, as extractMultipartModel does: a part that cannot be
	// read completely makes the whole form unusable (the proxy sees no model).
	reader := multipart.NewReader(bytes.NewReader(body), boundary)
	var names []string
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return body, nil
		}
		names = append(names, part.FormName())
		if _, err := io.Copy(io.Discard, part); err != nil {
			return body, nil
		}
	}
	protected := make([]int, len(names))
	keep := make([]int, len(protectedFormFields))
	for i := range keep {
		keep[i] = -1
	}
	counts := make([]int, len(protectedFormFields))
	needed := false
	for i, name := range names {
		protected[i] = protectedFormFieldIndex(name)
		p := protected[i]
		if p < 0 {
			continue
		}
		counts[p]++
		if name == protectedFormFields[p] {
			keep[p] = i
		}
		if counts[p] > 1 || name != protectedFormFields[p] {
			needed = true
		}
	}
	if !needed {
		return body, nil
	}
	removed := make([]int, len(protectedFormFields))
	drop := make([]bool, len(names))
	for i := range names {
		if p := protected[i]; p >= 0 && keep[p] != i {
			drop[i] = true
			removed[p]++
		}
	}
	// What upstream must read: the kept parts, as mime/multipart reads them.
	expected, ok := rawFormParts(body, boundary)
	if !ok || len(expected) != len(names) {
		return body, nil
	}
	kept := expected[:0:0]
	for i, part := range expected {
		if !drop[i] {
			kept = append(kept, part)
		}
	}
	var out []byte
	if segments, ok := splitMultipartSegments(body, boundary); ok && len(segments) == len(names) {
		out = make([]byte, 0, len(body))
		position := 0
		for i, segment := range segments {
			if drop[i] {
				out = append(out, body[position:segment[0]]...)
				position = segment[1]
			}
		}
		out = append(out, body[position:]...)
		if !sameFormParts(out, boundary, kept) {
			out = nil
		}
	}
	if out == nil {
		// A form whose byte layout the splitter does not reproduce (e.g. no
		// closing boundary, an empty part right after its headers) is rebuilt
		// part by part: kept parts keep their headers and content, only their
		// framing is re-serialized. Ambiguous protected fields are never
		// forwarded as is.
		if out, ok = rebuildMultipartWithout(body, boundary, drop); !ok || !sameFormParts(out, boundary, kept) {
			return body, nil
		}
	}
	var report []memberNormalization
	for p, n := range removed {
		if n > 0 {
			report = append(report, memberNormalization{Member: protectedFormFields[p], Removed: n})
		}
	}
	return out, report
}

type rawFormPart struct {
	name    string
	content []byte
}

// rawFormParts reads every part's field name and raw content strictly.
func rawFormParts(body []byte, boundary string) ([]rawFormPart, bool) {
	reader := multipart.NewReader(bytes.NewReader(body), boundary)
	var parts []rawFormPart
	for {
		part, err := reader.NextRawPart()
		if errors.Is(err, io.EOF) {
			return parts, true
		}
		if err != nil {
			return nil, false
		}
		content, err := io.ReadAll(part)
		if err != nil {
			return nil, false
		}
		parts = append(parts, rawFormPart{name: part.FormName(), content: content})
	}
}

// sameFormParts verifies an edited body reads as exactly the expected parts.
func sameFormParts(body []byte, boundary string, expected []rawFormPart) bool {
	got, ok := rawFormParts(body, boundary)
	if !ok || len(got) != len(expected) {
		return false
	}
	for i := range got {
		if got[i].name != expected[i].name || !bytes.Equal(got[i].content, expected[i].content) {
			return false
		}
	}
	return true
}

func rebuildMultipartWithout(body []byte, boundary string, drop []bool) ([]byte, bool) {
	reader := multipart.NewReader(bytes.NewReader(body), boundary)
	var out bytes.Buffer
	writer := multipart.NewWriter(&out)
	if writer.SetBoundary(boundary) != nil {
		return nil, false
	}
	i := 0
	for ; ; i++ {
		part, err := reader.NextRawPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil || i >= len(drop) {
			return nil, false
		}
		if drop[i] {
			if _, err := io.Copy(io.Discard, part); err != nil {
				return nil, false
			}
			continue
		}
		target, err := writer.CreatePart(part.Header)
		if err != nil {
			return nil, false
		}
		if _, err := io.Copy(target, part); err != nil {
			return nil, false
		}
	}
	if writer.Close() != nil || i != len(drop) {
		return nil, false
	}
	return out.Bytes(), true
}

// splitMultipartSegments locates every part as written: segment i runs from
// the start of its boundary line to the start of the next boundary line (the
// newline ending its content included), so removing whole segments leaves a
// well-formed body. Delimiter recognition follows mime/multipart: transport
// padding after the boundary, CRLF or (as set by the first boundary line) LF
// line endings, and a delimiter followed by LWSP, a newline or "--".
func splitMultipartSegments(body []byte, boundary string) ([][2]int, bool) {
	dash := []byte("--" + boundary)
	// The first boundary line may follow a preamble of whole lines.
	start, nl := -1, ""
	for pos := 0; pos < len(body); {
		end := bytes.IndexByte(body[pos:], '\n')
		if end < 0 {
			return nil, false
		}
		end += pos
		line := body[pos : end+1]
		if bytes.HasPrefix(line, dash) {
			rest := line[len(dash):]
			if bytes.HasPrefix(rest, []byte("--")) {
				return nil, true // final boundary first: no parts
			}
			rest = bytes.TrimLeft(rest, " \t")
			switch string(rest) {
			case "\r\n":
				start, nl = pos, "\r\n"
			case "\n":
				start, nl = pos, "\n"
			}
			if start >= 0 {
				break
			}
		}
		pos = end + 1
	}
	if start < 0 {
		return nil, false
	}
	delimiter := append([]byte(nl), dash...)
	var segments [][2]int
	lineStart := start
	content := start + bytes.IndexByte(body[start:], '\n') + 1
	for search := content; ; {
		found := bytes.Index(body[search:], delimiter)
		if found < 0 {
			return nil, false
		}
		at := search + found
		after := at + len(delimiter)
		if after >= len(body) {
			return nil, false
		}
		switch c := body[after]; {
		case c == ' ' || c == '\t' || c == '\r' || c == '\n':
		case c == '-' && after+1 < len(body) && body[after+1] == '-':
			segments = append(segments, [2]int{lineStart, at + len(nl)})
			return segments, true
		default:
			search = at + 1 // not a delimiter, keep scanning this part
			continue
		}
		end := bytes.IndexByte(body[after:], '\n')
		if end < 0 {
			return nil, false
		}
		end += after
		if string(bytes.TrimLeft(body[after:end+1], " \t")) != nl {
			return nil, false
		}
		next := at + len(nl)
		segments = append(segments, [2]int{lineStart, next})
		lineStart = next
		search = end + 1
	}
}

// detectMimeType 根据文件名后缀推断 MIME 类型。
func detectMimeType(filename string) string {
	filename = strings.ToLower(filename)
	switch {
	case strings.HasSuffix(filename, ".png"):
		return "image/png"
	case strings.HasSuffix(filename, ".jpg"), strings.HasSuffix(filename, ".jpeg"):
		return "image/jpeg"
	case strings.HasSuffix(filename, ".gif"):
		return "image/gif"
	case strings.HasSuffix(filename, ".webp"):
		return "image/webp"
	default:
		return "application/octet-stream"
	}
}
