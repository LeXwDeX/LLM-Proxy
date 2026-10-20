// request_body_kind.go — 按 Content-Type 精确媒体类型判定请求体形态，并拒绝不同解析器会读出不同字段的请求体。
package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/textproto"
	"strings"
	"unicode"
)

// requestBodyKind is how the proxy reads a request body: model extraction,
// normalization and multipart conversion all use classifyRequestBody so they
// read the same fields.
type requestBodyKind int

const (
	bodyJSON requestBodyKind = iota
	bodyURLEncoded
	bodyMultipart
)

const (
	ambiguousRequestBodyCode       = "ambiguous_request_body"
	unsupportedContentEncodingCode = "unsupported_content_encoding"
)

// hasRequestContentCoding reports whether the merged Content-Encoding fields
// of a request name any coding other than identity. Empty members are
// ignored; identity matches case-insensitively with surrounding whitespace.
func hasRequestContentCoding(values []string) bool {
	for _, value := range values {
		for _, coding := range strings.Split(value, ",") {
			if coding = strings.TrimSpace(coding); coding != "" && !strings.EqualFold(coding, "identity") {
				return true
			}
		}
	}
	return false
}

var (
	errMultipartContentType = errors.New("multipart content type parameters do not parse")
	// errURLEncodedJSONLike: a urlencoded body that looks like a JSON object
	// but is not one valid JSON object (see classifyRequestBody).
	errURLEncodedJSONLike = errors.New("urlencoded body looks like a JSON object")
)

const urlEncodedMediaType = "application/x-www-form-urlencoded"

// declaredMediaType parses a Content-Type with mime.ParseMediaType. Bad
// parameters (e.g. a duplicate boundary) leave mime without a media type; the
// declared type before ';' still decides how the upstream reads the body.
func declaredMediaType(contentType string) (mediaType string, params map[string]string, err error) {
	mediaType, params, err = mime.ParseMediaType(contentType)
	if err != nil && mediaType == "" {
		mediaType, _, _ = strings.Cut(contentType, ";")
		mediaType = strings.TrimSpace(mediaType)
	}
	return mediaType, params, err
}

// classifyRequestBody decides the body kind from the exact media type
// (mime.ParseMediaType), never from a substring of the header.
//   - application/x-www-form-urlencoded is a form unless the body, after
//     leading Unicode whitespace and UTF-8 BOMs, starts with '{' (curl -d
//     sends JSON with this type). Such a body is JSON when it is one valid
//     JSON value (json.Valid: no BOM, JSON whitespace only), whatever form-like
//     text its strings hold; the upstream is then told it is JSON
//     (urlEncodedJSONContentType). Otherwise err is errURLEncodedJSONLike and
//     the request is refused: a lenient JSON upstream (NaN, trailing commas,
//     a BOM) and the proxy's form reader would read different models.
//   - multipart/form-data is a form; boundary is its raw boundary parameter,
//     "" when absent. Every multipart reader (extraction, normalization,
//     conversion, model rewrite) uses this one value, never a trimmed copy:
//     mime/multipart and the upstream read the raw value. err is
//     errMultipartContentType when the parameters do not parse.
//   - everything else is JSON.
func classifyRequestBody(contentType string, body []byte) (kind requestBodyKind, boundary string, err error) {
	mediaType, params, parseErr := declaredMediaType(contentType)
	switch {
	case strings.EqualFold(mediaType, urlEncodedMediaType):
		if !looksLikeJSONObject(body) {
			return bodyURLEncoded, "", nil
		}
		if json.Valid(body) {
			return bodyJSON, "", nil
		}
		return bodyURLEncoded, "", errURLEncodedJSONLike
	case strings.EqualFold(mediaType, "multipart/form-data"):
		if parseErr != nil {
			// mime returns the media type with ErrInvalidMediaParameter; the
			// boundary is then unknown.
			return bodyMultipart, "", errMultipartContentType
		}
		return bodyMultipart, params["boundary"], nil
	}
	return bodyJSON, "", nil
}

// urlEncodedJSONContentType returns the Content-Type to forward for a
// urlencoded declaration whose body classifyRequestBody reads as JSON:
// application/json, keeping only a charset parameter, so the upstream reads
// the JSON the proxy authorized and billed instead of a form. ok is false for
// every other request, whose Content-Type is forwarded unchanged.
func urlEncodedJSONContentType(contentType string, body []byte) (rewritten string, ok bool) {
	mediaType, params, _ := declaredMediaType(contentType)
	if !strings.EqualFold(mediaType, urlEncodedMediaType) {
		return "", false
	}
	if kind, _, err := classifyRequestBody(contentType, body); err != nil || kind != bodyJSON {
		return "", false
	}
	if charset := params["charset"]; charset != "" {
		if formatted := mime.FormatMediaType("application/json", map[string]string{"charset": charset}); formatted != "" {
			return formatted, true
		}
	}
	return "application/json", true
}

// looksLikeJSONObject reports whether body starts with '{' after any Unicode
// whitespace and UTF-8 BOMs, which lenient JSON readers (JSON5, BOM-stripping
// decoders) skip.
func looksLikeJSONObject(body []byte) bool {
	trimmed := bytes.TrimLeftFunc(body, func(r rune) bool { return unicode.IsSpace(r) || r == '\uFEFF' })
	return len(trimmed) > 0 && trimmed[0] == '{'
}

// requestBodyAmbiguity returns a client-facing reason when a declared
// multipart/form-data or urlencoded body could be read differently by a form
// parser and a JSON parser, or its field names differently by different form
// parsers; "" otherwise. Valid clients never send these shapes.
func requestBodyAmbiguity(contentType string, body []byte) string {
	kind, boundary, err := classifyRequestBody(contentType, body)
	if errors.Is(err, errURLEncodedJSONLike) {
		return "application/x-www-form-urlencoded body looks like a JSON object but is not valid JSON"
	}
	if kind != bodyMultipart {
		return ""
	}
	if err != nil {
		return "multipart/form-data content type parameters do not parse"
	}
	if trimmed := bytes.TrimLeft(body, " \t\r\n"); len(trimmed) > 0 && trimmed[0] == '{' {
		return "multipart/form-data body starts with a JSON object"
	}
	if len(body) == 0 {
		return ""
	}
	if boundary == "" {
		return "multipart/form-data content type has no boundary"
	}
	if boundary != strings.TrimSpace(boundary) {
		// Readers that trim the parameter and readers that do not (Go's
		// mime/multipart) would split the body at different lines.
		return "multipart/form-data boundary has surrounding whitespace"
	}
	if reason := boundaryParameterAmbiguity(contentType, boundary); reason != "" {
		return reason
	}
	if len(bytes.Trim(body[:multipartPreambleEnd(body, boundary)], " \t\r\n")) > 0 {
		return "multipart/form-data body has content before the first boundary"
	}
	if multipartStrayDelimiter(body, boundary) {
		return "multipart/form-data body has a line starting with the boundary that is not a delimiter"
	}
	reader := multipart.NewReader(bytes.NewReader(body), boundary)
	for {
		part, err := reader.NextRawPart()
		if err != nil {
			// The end, or a form that does not parse: the proxy then reads no
			// fields at all (extractMultipartModel), which is not ambiguous.
			return ""
		}
		if reason := formFieldNameAmbiguity(part.Header.Values("Content-Disposition")); reason != "" {
			return reason
		}
		if part.FileName() == "" && protectedFormFieldIndex(part.FormName()) >= 0 {
			if reason := protectedFieldEncodingAmbiguity(part.Header); reason != "" {
				return reason
			}
		}
		if _, err := io.Copy(io.Discard, part); err != nil {
			return ""
		}
	}
}

// protectedFieldEncodingAmbiguity checks the headers of a protected form
// field (not a file part): its value must be the raw UTF-8 bytes every reader
// takes. The proxy reads raw part bytes; mime/multipart's NextPart decodes
// quoted-printable, busboy decodes the part charset. So a field with any
// Content-Transfer-Encoding, a charset other than UTF-8/US-ASCII, or a
// Content-Type that does not parse (or appears more than once) is refused.
func protectedFieldEncodingAmbiguity(header textproto.MIMEHeader) string {
	if len(header.Values("Content-Transfer-Encoding")) > 0 {
		return "multipart/form-data protected field has a Content-Transfer-Encoding"
	}
	contentTypes := header.Values("Content-Type")
	if len(contentTypes) == 0 {
		return ""
	}
	if len(contentTypes) > 1 {
		return "multipart/form-data protected field has more than one Content-Type"
	}
	_, params, err := mime.ParseMediaType(contentTypes[0])
	if err != nil {
		return "multipart/form-data protected field has an unparseable Content-Type"
	}
	if charset, ok := params["charset"]; ok && !strings.EqualFold(charset, "utf-8") && !strings.EqualFold(charset, "us-ascii") {
		return "multipart/form-data protected field has a charset other than UTF-8"
	}
	return ""
}

// boundaryParameterAmbiguity checks the boundary parameter as written in the
// Content-Type, not as mime unescapes or reassembles it (boundary is mime's
// value). Parsers disagree on backslash escapes in quoted strings (busboy
// unescapes differently), on RFC 2231 boundary* parameters (mime lets them
// override boundary=, busboy takes the first boundary=) and on characters
// outside RFC 2046; a boundary any of them reads differently splits the body
// at other lines, past the delimiter checks, which use mime's value. So the
// raw parameter must be one plain RFC 2046 boundary equal to mime's value.
func boundaryParameterAmbiguity(contentType, boundary string) string {
	raw, found := "", false
	for _, param := range rawMediaParams(contentType) {
		if strings.HasPrefix(param.key, "boundary*") {
			return "multipart/form-data content type uses an RFC 2231 boundary parameter"
		}
		if param.key == "boundary" {
			if param.quoted && strings.Contains(param.value, `\`) {
				return "multipart/form-data boundary has a backslash escape"
			}
			raw, found = param.value, true
		}
	}
	if !found || raw != boundary || !rfc2046Boundary(boundary) {
		return "multipart/form-data boundary is not a plain RFC 2046 boundary"
	}
	return ""
}

// rfc2046Boundary reports whether boundary is 1 to 70 bchars (DIGIT, ALPHA,
// '()+_,-./:=? and space) not ending in a space (RFC 2046 section 5.1.1).
func rfc2046Boundary(boundary string) bool {
	if len(boundary) == 0 || len(boundary) > 70 || boundary[len(boundary)-1] == ' ' {
		return false
	}
	for i := 0; i < len(boundary); i++ {
		switch c := boundary[i]; {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9':
		case strings.IndexByte("'()+_,-./:=? ", c) >= 0:
		default:
			return false
		}
	}
	return true
}

// rawMediaParam is a header parameter as written: the lower-cased key and
// the value, a quoted value without its quotes and not unescaped.
type rawMediaParam struct {
	key, value string
	quoted     bool
}

// rawMediaParams returns the parameters of a header value mime already
// parses, as written, skipping quoted strings so a ';' or '=' inside a
// quoted value is not a separator.
func rawMediaParams(value string) []rawMediaParam {
	var params []rawMediaParam
	_, rest, found := strings.Cut(value, ";")
	for found {
		rest = strings.TrimLeftFunc(rest, unicode.IsSpace)
		equals := strings.IndexByte(rest, '=')
		if equals < 0 {
			return params
		}
		param := rawMediaParam{key: strings.ToLower(strings.TrimFunc(rest[:equals], unicode.IsSpace))}
		rest = strings.TrimLeftFunc(rest[equals+1:], unicode.IsSpace)
		if strings.HasPrefix(rest, `"`) {
			i := 1
			for i < len(rest) && rest[i] != '"' {
				if rest[i] == '\\' {
					i++
				}
				i++
			}
			if i >= len(rest) {
				return params
			}
			param.value, param.quoted = rest[1:i], true
			rest = rest[i+1:]
		} else {
			end := strings.IndexByte(rest, ';')
			if end < 0 {
				end = len(rest)
			}
			param.value = strings.TrimRightFunc(rest[:end], unicode.IsSpace)
			rest = rest[end:]
		}
		params = append(params, param)
		_, rest, found = strings.Cut(rest, ";")
	}
	return params
}

// formFieldNameAmbiguity checks a part's Content-Disposition headers: exactly
// one, form-data, parseable, without RFC 2231 name parameters (name*=,
// name*0=) and without duplicate parameters, so every form parser reads the
// same field name as part.FormName.
func formFieldNameAmbiguity(values []string) string {
	if len(values) != 1 {
		return "multipart/form-data part needs exactly one Content-Disposition header"
	}
	disposition, _, err := mime.ParseMediaType(values[0])
	if err != nil {
		// Duplicate parameters (case-insensitively) fail here too.
		return "multipart/form-data part has an unparseable Content-Disposition"
	}
	if disposition != "form-data" {
		return "multipart/form-data part Content-Disposition is not form-data"
	}
	for _, param := range rawMediaParams(values[0]) {
		if strings.HasPrefix(param.key, "name*") {
			return "multipart/form-data part uses an RFC 2231 field name"
		}
	}
	return ""
}

// multipartPreambleEnd returns where the first line mime/multipart accepts as
// a boundary starts (a delimiter line or the closing boundary), or len(body):
// everything before it is the preamble mime/multipart skips silently.
func multipartPreambleEnd(body []byte, boundary string) int {
	dash := []byte("--" + boundary)
	for start := 0; start < len(body); {
		end := bytes.IndexByte(body[start:], '\n')
		if end < 0 {
			end = len(body)
		} else {
			end += start + 1
		}
		line := body[start:end]
		if bytes.HasPrefix(line, dash) {
			rest := line[len(dash):]
			final := bytes.HasPrefix(rest, []byte("--"))
			if final {
				rest = rest[2:]
			}
			rest = bytes.TrimLeft(rest, " \t")
			// mime/multipart: a delimiter line ends in CRLF or LF; before any
			// part the closing boundary must end in CRLF or the body.
			if !final && (string(rest) == "\r\n" || string(rest) == "\n") {
				return start
			}
			if final && (len(rest) == 0 || string(rest) == "\r\n") {
				return start
			}
		}
		start = end
	}
	return len(body)
}

// multipartStrayDelimiter reports a line starting with "--"+boundary that
// mime/multipart does not use as a delimiter: other text after the boundary
// (beyond LWSP and the line break, or "--" for the closing line), a line
// break of the other style than the first delimiter line set (CRLF forms
// split only at CRLF, LF forms only at LF), or any such line after the
// closing boundary. Other parsers (busboy splits at "\r\n--"+boundary
// without checking the rest of the line) could start a part there that the
// proxy never read. RFC 2046 forbids such lines inside parts, so valid forms
// never contain them. In a CRLF form it also reports a part whose header
// block (up to and including its empty line) has a line not ending in CRLF:
// textproto ends the headers at a bare LF line (and mime/multipart then
// accepts a delimiter right after it), busboy does not and merges parts. It
// is one bytes.Index pass over the body plus one walk of each header block.
func multipartStrayDelimiter(body []byte, boundary string) bool {
	first := multipartPreambleEnd(body, boundary)
	if first == len(body) {
		return false // no delimiter at all: the preamble check decides
	}
	dash := []byte("--" + boundary)
	// The first delimiter line sets the line break style, as in
	// mime/multipart; a closing line first ends the form.
	nl := []byte("\r\n")
	firstLine := body[first+len(dash):]
	closed := bytes.HasPrefix(firstLine, []byte("--"))
	if !closed && bytes.HasPrefix(bytes.TrimLeft(firstLine, " \t"), []byte("\n")) {
		nl = nl[1:]
	}
	// headers is where the current part's header block starts.
	headers := lineEnd(body, first)
	for pos := first + len(dash); pos < len(body); {
		i := bytes.Index(body[pos:], dash)
		if i < 0 {
			break
		}
		at := pos + i
		pos = at + len(dash)
		if body[at-1] != '\n' {
			continue // not at a line start (at > first >= 0)
		}
		if closed {
			return true
		}
		if len(nl) == 2 && (body[at-2] != '\r' || headerBlockNotCRLF(body[headers:at])) {
			return true
		}
		final, ok := multipartDelimiterLine(body[pos:], nl)
		if !ok {
			return true
		}
		closed = final
		headers = lineEnd(body, at)
	}
	return !closed && len(nl) == 2 && headerBlockNotCRLF(body[headers:])
}

// lineEnd returns the index after the line break of the line containing
// start, or len(body).
func lineEnd(body []byte, start int) int {
	if end := bytes.IndexByte(body[start:], '\n'); end >= 0 {
		return start + end + 1
	}
	return len(body)
}

// headerBlockNotCRLF reports whether a part's header block, the lines from
// the start of part up to and including the first empty line, has a line
// that ends in a bare LF. A last line without a line break ends part.
func headerBlockNotCRLF(part []byte) bool {
	for start := 0; start < len(part); {
		end := bytes.IndexByte(part[start:], '\n')
		if end < 0 {
			return false
		}
		if end == 0 || part[start+end-1] != '\r' {
			return true
		}
		if end == 1 {
			return false // the empty CRLF line ends the headers
		}
		start += end + 1
	}
	return false
}

// multipartDelimiterLine checks the rest of a line after "--"+boundary the
// way mime/multipart does: a delimiter is LWSP then the line break; the
// closing line is "--", LWSP, then the line break or the end of the body.
func multipartDelimiterLine(rest, nl []byte) (final, ok bool) {
	if end := bytes.IndexByte(rest, '\n'); end >= 0 {
		rest = rest[:end+1]
	}
	if bytes.HasPrefix(rest, []byte("--")) {
		rest = bytes.TrimLeft(rest[2:], " \t")
		return true, len(rest) == 0 || bytes.Equal(rest, nl)
	}
	return false, bytes.Equal(bytes.TrimLeft(rest, " \t"), nl)
}
