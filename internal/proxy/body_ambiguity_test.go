package proxy

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/tracestore"
)

func serveWithContentType(s *Service, principal *auth.Principal, path, contentType, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	r.Header.Set("Content-Type", contentType)
	r = r.WithContext(auth.WithPrincipal(r.Context(), principal))
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func (c *capturedUpstream) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.bodies)
}

func assertAmbiguousBody(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s, want 400 ambiguous_request_body", w.Code, w.Body.String())
	}
	var response requestError
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil || response.Error.Code != "ambiguous_request_body" || response.Error.Type != "invalid_request_error" {
		t.Fatalf("error body %s", w.Body.String())
	}
}

const urlEncodedType = "application/x-www-form-urlencoded"

// A JSON object sent with the urlencoded content type (curl -d) is read as
// JSON only: a "&model=" hidden in a JSON string is not a form field.
func TestURLEncodedHeaderJSONBodyIsJSON(t *testing.T) {
	s, captured, principal := newNormalizeHTTPService(t, []config.ModelMapping{{Upstream: "test-model"}})
	for _, body := range []string{
		`{"model":"gpt-secret","x":"&model=test-model&y=","messages":[]}`,
		" \r\n\t" + `{"model":"gpt-secret","messages":[{"content":"a&model=test-model"}]}`,
		`{"model":"test-model","model":"gpt-secret","x":"&model=test-model"}`,
	} {
		before := captured.count()
		w := serveWithContentType(s, principal, "/v1/chat/completions", urlEncodedType, body)
		if w.Code < 400 || w.Code >= 500 || captured.count() != before {
			t.Fatalf("%s: status=%d forwarded=%v", body, w.Code, captured.count() != before)
		}
	}
	// curl -d style requests keep working, JSON rules included.
	for _, tc := range []struct{ body, want string }{
		{`{"model":"test-model","messages":[]}`, `{"model":"test-model","messages":[]}`},
		{`{"model":"gpt-secret","model":"test-model","x":"&model=gpt-secret"}`, `{"model":"test-model","x":"&model=gpt-secret"}`},
	} {
		w := serveWithContentType(s, principal, "/v1/chat/completions", urlEncodedType+"; charset=utf-8", tc.body)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: status=%d body=%s", tc.body, w.Code, w.Body.String())
		}
		if sent := captured.last(t); string(sent) != tc.want {
			t.Fatalf("upstream got %s, want %s", sent, tc.want)
		}
		if got := captured.lastHeader(t).Get("Content-Type"); got != "application/json; charset=utf-8" {
			t.Fatalf("upstream Content-Type %q, want application/json; charset=utf-8", got)
		}
	}
	// A real form is still read as a form.
	if w := serveWithContentType(s, principal, "/v1/chat/completions", urlEncodedType, "model=test-model&messages=x"); w.Code != http.StatusOK {
		t.Fatalf("form: status=%d", w.Code)
	}
}

// A multipart declaration over a JSON body, or with content before the first
// boundary, is refused: a JSON reader and a form reader see different models.
func TestMultipartAmbiguousBodyRejected(t *testing.T) {
	s, captured, principal := newNormalizeHTTPService(t, []config.ModelMapping{{Upstream: "test-model"}})
	form := "--b\r\nContent-Disposition: form-data; name=\"model\"\r\n\r\ntest-model\r\n--b--\r\n"
	for _, tc := range []struct{ name, contentType, body string }{
		{"JSON before boundary", "multipart/form-data; boundary=b", `{"model":"gpt-secret","messages":[]}` + "\r\n" + form},
		{"JSON only", "multipart/form-data; boundary=b", ` {"model":"test-model"}`},
		{"JSON without boundary", "multipart/form-data", `{"model":"test-model"}`},
		{"text preamble", "multipart/form-data; boundary=b", "model=gpt-secret\r\n" + form},
		{"LF closing boundary first is preamble", "multipart/form-data; boundary=b", "--b--\n" + form},
		{"invalid content type parameters", "multipart/form-data; boundary=b; boundary=c", form},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := captured.count()
			assertAmbiguousBody(t, serveWithContentType(s, principal, "/v1/chat/completions", tc.contentType, tc.body))
			if captured.count() != before {
				t.Fatal("ambiguous body forwarded")
			}
		})
	}
	// Whitespace before the first boundary is fine.
	for _, body := range []string{form, "\r\n \t\r\n" + form, "\n\n" + strings.ReplaceAll(form, "\r\n", "\n")} {
		if w := serveWithContentType(s, principal, "/v1/chat/completions", "multipart/form-data; boundary=b", body); w.Code != http.StatusOK {
			t.Fatalf("%q: status=%d body=%s", body, w.Code, w.Body.String())
		}
	}
}

// Field names other form parsers could read differently are refused.
func TestMultipartAmbiguousFieldNamesRejected(t *testing.T) {
	s, captured, principal := newNormalizeHTTPService(t, []config.ModelMapping{{Upstream: "test-model"}})
	part := func(headers string) string {
		return "--b\r\nContent-Disposition: form-data; name=\"model\"\r\n\r\ntest-model\r\n" +
			"--b\r\n" + headers + "\r\n\r\nvalue\r\n--b--\r\n"
	}
	for _, tc := range []struct{ name, headers string }{
		{"RFC 2231 name", `Content-Disposition: form-data; name*=utf-8''model`},
		{"RFC 2231 name continuation", `Content-Disposition: form-data; name*0="mo"; name*1="del"`},
		{"RFC 2231 name beside name", `Content-Disposition: form-data; name="x"; name*=utf-8''model`},
		{"duplicate name", `Content-Disposition: form-data; name="model"; name="x"`},
		{"duplicate name case", `Content-Disposition: form-data; NAME="x"; name="model"`},
		{"unterminated quote", `Content-Disposition: form-data; name="model`},
		{"no media type", `Content-Disposition: ; name="model"`},
		{"missing", `Content-Type: text/plain`},
		{"two headers", "Content-Disposition: form-data; name=\"x\"\r\nContent-Disposition: form-data; name=\"model\""},
		{"not form-data", `Content-Disposition: attachment; name="model"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := captured.count()
			assertAmbiguousBody(t, serveWithContentType(s, principal, "/v1/chat/completions", "multipart/form-data; boundary=b", part(tc.headers)))
			if captured.count() != before {
				t.Fatal("ambiguous body forwarded")
			}
		})
	}
	for _, headers := range []string{
		`Content-Disposition: form-data; name="image"; filename="a.png"` + "\r\nContent-Type: image/png",
		`Content-Disposition: Form-Data; Name="prompt"`,
		`Content-Disposition: form-data; name="image"; filename*=utf-8''a.png`,
	} {
		if w := serveWithContentType(s, principal, "/v1/chat/completions", "multipart/form-data; boundary=b", part(headers)); w.Code != http.StatusOK {
			t.Fatalf("%s: status=%d body=%s", headers, w.Code, w.Body.String())
		}
	}
}

var preambleFuzzCorpus = []string{
	"--" + multipartTestBoundary + "\r\nContent-Disposition: form-data; name=\"model\"\r\n\r\nm\r\n--" + multipartTestBoundary + "--\r\n",
	"pre\r\n--" + multipartTestBoundary + " \t\r\n\r\nx\r\n--" + multipartTestBoundary + "--",
	"\n--" + multipartTestBoundary + "\n\nx\n--" + multipartTestBoundary + "--\n",
	"--" + multipartTestBoundary + "--\n--" + multipartTestBoundary + "\r\n\r\nx\r\n--" + multipartTestBoundary + "--\r\n",
	"--" + multipartTestBoundary + "-- \r\n",
	"{\"model\":\"m\"}",
}

// multipartPreambleEnd must find exactly the preamble mime/multipart skips:
// reading from there gives the same parts, and with no boundary found
// mime/multipart reads no part.
func checkMultipartPreamble(t *testing.T, body []byte) {
	t.Helper()
	end := multipartPreambleEnd(body, multipartTestBoundary)
	names, values, ok := goMultipartParts(body, multipartTestBoundary)
	if end == len(body) {
		if ok && len(names) > 0 {
			t.Fatalf("%q: no boundary found but mime/multipart read %v", body, names)
		}
		return
	}
	restNames, restValues, restOK := goMultipartParts(body[end:], multipartTestBoundary)
	if ok != restOK || strings.Join(names, "\x00") != strings.Join(restNames, "\x00") || strings.Join(values, "\x00") != strings.Join(restValues, "\x00") {
		t.Fatalf("%q: preamble end %d changes the parts: %v %v %v vs %v %v %v", body, end, names, values, ok, restNames, restValues, restOK)
	}
}

func TestMultipartPreambleMatchesMime(t *testing.T) {
	for _, body := range preambleFuzzCorpus {
		checkMultipartPreamble(t, []byte(body))
	}
}

func FuzzMultipartPreamble(f *testing.F) {
	for _, body := range preambleFuzzCorpus {
		f.Add([]byte(body))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		checkMultipartPreamble(t, body)
	})
}

// extractModel and normalization classify bodies by the exact media type.
func TestRequestBodyKindExactMediaType(t *testing.T) {
	for _, tc := range []struct {
		contentType, body string
		kind              requestBodyKind
		boundary          string
	}{
		{"application/json", `{"model":"m"}`, bodyJSON, ""},
		{"", "model=m", bodyJSON, ""},
		{"application/x-www-form-urlencoded", "model=m", bodyURLEncoded, ""},
		{"Application/X-WWW-Form-Urlencoded; charset=utf-8", "model=m", bodyURLEncoded, ""},
		{"application/x-www-form-urlencoded", ` {"model":"m"}`, bodyJSON, ""},
		{"application/x-www-form-urlencoded", `{"model":"m"`, bodyURLEncoded, ""},
		{"application/x-www-form-urlencoded", `["model=m"]`, bodyURLEncoded, ""},
		{"text/plain; x=application/x-www-form-urlencoded", "model=m", bodyJSON, ""},
		{"text/plain; x=\"multipart/form-data; boundary=b\"", "model=m", bodyJSON, ""},
		{`Multipart/Form-Data; boundary=" b "`, "", bodyMultipart, " b "},
		{"multipart/mixed; boundary=b", "", bodyJSON, ""},
	} {
		kind, boundary, _ := classifyRequestBody(tc.contentType, []byte(tc.body))
		if kind != tc.kind || boundary != tc.boundary {
			t.Fatalf("%q %q: kind=%v boundary=%q, want %v %q", tc.contentType, tc.body, kind, boundary, tc.kind, tc.boundary)
		}
	}
	// extractModel follows the classification.
	for _, tc := range []struct{ contentType, body, want string }{
		{urlEncodedType, `{"model":"json","x":"&model=form"}`, "json"},
		{urlEncodedType, "model=form", "form"},
		{"text/plain; x=application/x-www-form-urlencoded", "model=form", ""},
		{"text/plain; x=application/x-www-form-urlencoded", `{"model":"json"}`, "json"},
	} {
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(tc.body))
		r.Header.Set("Content-Type", tc.contentType)
		if got := extractModel(r, []byte(tc.body)); got != tc.want {
			t.Fatalf("%q %q: model %q, want %q", tc.contentType, tc.body, got, tc.want)
		}
	}
}

// boundaryWhitespaceBody is read as model=allowed by a reader that trims the
// boundary parameter and as model=forbidden by mime/multipart on the raw
// parameter (and by the upstream).
func boundaryWhitespaceBody(raw string) string {
	trimmed := strings.TrimSpace(raw)
	part := func(boundary, model string) string {
		return "--" + boundary + "\r\nContent-Disposition: form-data; name=\"model\"\r\n\r\n" + model + "\r\n--" + boundary + "--\r\n"
	}
	return part(trimmed, "test-model") + part(raw, "gpt-secret")
}

// H1: a boundary parameter with surrounding whitespace (or an empty one) is
// refused: readers disagree whether to trim it and so read different parts.
func TestMultipartBoundaryWhitespaceRejected(t *testing.T) {
	s, captured, principal := newNormalizeHTTPService(t, []config.ModelMapping{{Upstream: "test-model"}})
	for _, tc := range []struct{ name, contentType, body string }{
		{"leading space", `multipart/form-data; boundary=" abc"`, boundaryWhitespaceBody(" abc")},
		{"leading tab", "multipart/form-data; boundary=\"\tabc\"", boundaryWhitespaceBody("\tabc")},
		{"trailing space", `multipart/form-data; boundary="abc "`, boundaryWhitespaceBody("abc ")},
		{"both sides", `multipart/form-data; boundary=" abc "`, boundaryWhitespaceBody(" abc ")},
		{"empty boundary", `multipart/form-data; boundary=""`, boundaryWhitespaceBody("abc")},
		{"missing boundary", `multipart/form-data`, boundaryWhitespaceBody("abc")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := captured.count()
			assertAmbiguousBody(t, serveWithContentType(s, principal, "/v1/chat/completions", tc.contentType, tc.body))
			if captured.count() != before {
				t.Fatal("ambiguous body forwarded")
			}
		})
	}
	// A quoted boundary without surrounding whitespace keeps working.
	form := "--a b\r\nContent-Disposition: form-data; name=\"model\"\r\n\r\ntest-model\r\n--a b--\r\n"
	if w := serveWithContentType(s, principal, "/v1/chat/completions", `multipart/form-data; boundary="a b"`, form); w.Code != http.StatusOK {
		t.Fatalf("quoted boundary: status=%d body=%s", w.Code, w.Body.String())
	}
}

// Every multipart consumer uses the one raw boundary classifyRequestBody
// returns, never a separately trimmed copy.
func TestMultipartBoundaryRawEverywhere(t *testing.T) {
	const contentType = `multipart/form-data; boundary=" abc"`
	body := []byte(boundaryWhitespaceBody(" abc"))
	_, boundary, err := classifyRequestBody(contentType, body)
	if err != nil || boundary != " abc" {
		t.Fatalf("boundary %q err %v, want raw \" abc\"", boundary, err)
	}
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body)))
	r.Header.Set("Content-Type", contentType)
	// extractModel reads what mime/multipart on the raw boundary reads.
	if got := extractModel(r, body); got != "gpt-secret" {
		t.Fatalf("extractModel %q, want the raw-boundary reading gpt-secret", got)
	}
}

// M1: with the urlencoded content type, a body that looks like a JSON object
// (after whitespace and a UTF-8 BOM) is JSON only when it is valid JSON
// without a BOM; anything else is refused, so a lenient JSON upstream and the
// proxy's form reader cannot disagree.
func TestURLEncodedJSONLikeBodyRejected(t *testing.T) {
	s, captured, principal := newNormalizeHTTPService(t, []config.ModelMapping{{Upstream: "test-model"}})
	const bom = "\xef\xbb\xbf"
	for _, tc := range []struct{ name, body string }{
		{"NaN", `{"model":"gpt-secret","t":NaN,"x":"&model=test-model&y="}`},
		{"trailing comma", `{"model":"gpt-secret","x":"&model=test-model&y=",}`},
		{"BOM", bom + `{"model":"gpt-secret","x":"&model=test-model&y="}`},
		{"whitespace then BOM", " \r\n" + bom + `{"model":"gpt-secret","x":"&model=test-model&y="}`},
		{"BOM valid JSON", bom + `{"model":"test-model","messages":[]}`},
		{"form feed", "\f" + `{"model":"test-model","messages":[]}`},
		{"unterminated", `{"model":"test-model"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := captured.count()
			assertAmbiguousBody(t, serveWithContentType(s, principal, "/v1/chat/completions", urlEncodedType, tc.body))
			if captured.count() != before {
				t.Fatal("ambiguous body forwarded")
			}
		})
	}
	// Valid JSON is JSON whatever its strings hold (form-like "&model=" text
	// included): forwarded unchanged and declared as JSON upstream, so the
	// upstream reads the same JSON. Plain forms keep working as forms.
	for _, tc := range []struct{ body, want, contentType string }{
		{`{"model":"test-model","x":"&foo=bar;baz=1","messages":[]}`, `{"model":"test-model","x":"&foo=bar;baz=1","messages":[]}`, "application/json"},
		{" \r\n" + `{"model":"test-model"}`, " \r\n" + `{"model":"test-model"}`, "application/json"},
		{`{"model":"test-model","x":"&model=gpt-secret&y="}`, `{"model":"test-model","x":"&model=gpt-secret&y="}`, "application/json"},
		{`{"model":"test-model","x":"&MODEL=gpt-secret;model[]=x&%6dodel=y&stream=true"}`, `{"model":"test-model","x":"&MODEL=gpt-secret;model[]=x&%6dodel=y&stream=true"}`, "application/json"},
		// curl -d with a URL in a message.
		{`{"model":"test-model","messages":[{"role":"user","content":"see https://x.io/?a=1&n=2"}]}`, `{"model":"test-model","messages":[{"role":"user","content":"see https://x.io/?a=1&n=2"}]}`, "application/json"},
		{"model=test-model&messages=x", "model=test-model&messages=x", urlEncodedType},
	} {
		w := serveWithContentType(s, principal, "/v1/chat/completions", urlEncodedType, tc.body)
		if w.Code != http.StatusOK {
			t.Fatalf("%q: status=%d body=%s", tc.body, w.Code, w.Body.String())
		}
		if sent := captured.last(t); string(sent) != tc.want {
			t.Fatalf("upstream got %s, want %s", sent, tc.want)
		}
		if got := captured.lastHeader(t).Get("Content-Type"); got != tc.contentType {
			t.Fatalf("%q: upstream Content-Type %q, want %q", tc.body, got, tc.contentType)
		}
	}
	// A JSON model the client may not use is refused, whatever form-like
	// text its strings hold.
	before := captured.count()
	if w := serveWithContentType(s, principal, "/v1/chat/completions", urlEncodedType, `{"model":"gpt-secret","x":"&model=test-model&y="}`); w.Code < 400 || w.Code >= 500 || captured.count() != before {
		t.Fatalf("hidden form model: status=%d forwarded=%v", w.Code, captured.count() != before)
	}
}

// The upstream Content-Type for a urlencoded declaration read as JSON is
// application/json, keeping only a charset parameter; it is set for every
// attempt (failover included) and the trace records what was sent.
func TestURLEncodedJSONUpstreamContentType(t *testing.T) {
	for _, tc := range []struct{ contentType, want string }{
		{urlEncodedType, "application/json"},
		{"Application/X-WWW-Form-Urlencoded; charset=UTF-8", "application/json; charset=UTF-8"},
		{urlEncodedType + "; foo=bar; Charset=\"latin1\"", "application/json; charset=latin1"},
		{urlEncodedType + "; foo=bar", "application/json"},
	} {
		got, ok := urlEncodedJSONContentType(tc.contentType, []byte(`{"model":"m"}`))
		if !ok || got != tc.want {
			t.Fatalf("%q: %q %v, want %q", tc.contentType, got, ok, tc.want)
		}
	}
	for _, tc := range []struct{ contentType, body string }{
		{urlEncodedType, "model=m"},
		{urlEncodedType, `{"model":"m",}`},
		{"application/json", `{"model":"m"}`},
		{"text/plain", `{"model":"m"}`},
		{"multipart/form-data; boundary=b", `{"model":"m"}`},
	} {
		if got, ok := urlEncodedJSONContentType(tc.contentType, []byte(tc.body)); ok {
			t.Fatalf("%q %q: rewritten to %q", tc.contentType, tc.body, got)
		}
	}

	// Failover: the first attempt fails at the dial, the second target
	// answers; both attempts declare JSON, and the trace matches.
	var mu sync.Mutex
	var hosts, contentTypes []string
	s, err := NewService(&config.Config{
		Server: config.ServerConfig{RequestTimeoutSeconds: 5},
		Targets: []config.Target{
			{Name: "a", EndpointType: config.EndpointTypeOpenAI, Endpoint: "http://a.invalid", APIKey: "fixture", UsageAdapter: "standard", ModelMappings: []config.ModelMapping{{Upstream: "test-model"}}},
			{Name: "b", EndpointType: config.EndpointTypeOpenAI, Endpoint: "http://b.invalid", APIKey: "fixture", UsageAdapter: "standard", ModelMappings: []config.ModelMapping{{Upstream: "test-model"}}},
		},
	}, newTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		defer mu.Unlock()
		hosts = append(hosts, r.URL.Host)
		contentTypes = append(contentTypes, r.Header.Get("Content-Type"))
		if len(hosts) == 1 {
			return nil, errors.New("dial error")
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"usage":{"prompt_tokens":1,"completion_tokens":1}}`)), Request: r}, nil
	})}
	store := tracestore.New(tracestore.Config{Enabled: true, RingBufferSize: 1, MaxBodySize: 4096, ChannelBuffer: 1}, newTestLogger())
	defer store.Close()
	s.traceStore = store
	w := serveWithContentType(s, newAllowAllPrincipal(t, "fixture-urlencoded-json"), "/v1/chat/completions", urlEncodedType+"; charset=utf-8", `{"model":"test-model","x":"&model=gpt-secret"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(hosts) != 2 || hosts[0] == hosts[1] {
		t.Fatalf("attempts %v, want a failover to the other target", hosts)
	}
	for i, got := range contentTypes {
		if got != "application/json; charset=utf-8" {
			t.Fatalf("attempt %d Content-Type %q", i, got)
		}
	}
	trace := store.Get("")
	if trace == nil {
		t.Fatal("missing trace")
	}
	if got := trace.RequestHeaders["Content-Type"]; got != "application/json; charset=utf-8" {
		t.Fatalf("trace Content-Type %q, want the one sent upstream", got)
	}
}

// twoBoundaryForm is read as model=test-model by mime/multipart on goBoundary
// and as model=gpt-secret by a parser that takes otherBoundary from the same
// Content-Type (busboy: its own unescaping, the first boundary parameter, no
// RFC 2231): the second form is Go's epilogue. With one boundary it is one
// clean form, so only the Content-Type can be refused.
func twoBoundaryForm(goBoundary, otherBoundary string) string {
	part := func(boundary, model string) string {
		return "--" + boundary + "\r\nContent-Disposition: form-data; name=\"model\"\r\n\r\n" + model + "\r\n--" + boundary + "--\r\n"
	}
	if otherBoundary == goBoundary {
		return part(goBoundary, "test-model")
	}
	return part(goBoundary, "test-model") + part(otherBoundary, "gpt-secret")
}

// H2: the boundary parameter is checked as written in the Content-Type, not
// as mime unescapes or reassembles it: a backslash escape in a quoted
// boundary, any RFC 2231 boundary* parameter, or a value outside RFC 2046
// bchars is refused, since parsers then pick different boundaries.
func TestMultipartBoundaryParameterRejected(t *testing.T) {
	s, captured, principal := newNormalizeHTTPService(t, []config.ModelMapping{{Upstream: "test-model"}})
	for _, tc := range []struct{ name, contentType, body string }{
		{"escaped paren", `multipart/form-data; boundary="AaB\(x"`, twoBoundaryForm("AaB(x", `AaB\(x`)},
		{"escaped letter", `multipart/form-data; boundary="A\Bc"`, twoBoundaryForm(`A\Bc`, "ABc")},
		{"escaped quote", `multipart/form-data; boundary="a\"b"`, twoBoundaryForm(`a"b`, `a"b`)},
		{"RFC 2231 after plain", `multipart/form-data; boundary=AAA; boundary*=utf-8''BBB`, twoBoundaryForm("BBB", "AAA")},
		{"RFC 2231 continuation before plain", `multipart/form-data; boundary*0=BB; boundary*1=B; boundary=AAA`, twoBoundaryForm("BBB", "AAA")},
		{"RFC 2231 only", `multipart/form-data; BOUNDARY*=utf-8''BBB`, twoBoundaryForm("BBB", "BBB")},
		{"RFC 2231 continuation only", `multipart/form-data; Boundary*0="BB"; boundary*1="B"`, twoBoundaryForm("BBB", "BBB")},
		{"not a bchar", `multipart/form-data; boundary="a@b"`, twoBoundaryForm("a@b", "a@b")},
		{"tab inside", "multipart/form-data; boundary=\"a\tb\"", twoBoundaryForm("a\tb", "a\tb")},
		{"non-ASCII", `multipart/form-data; boundary="aäb"`, twoBoundaryForm("aäb", "aäb")},
		{"too long", "multipart/form-data; boundary=" + strings.Repeat("x", 71), twoBoundaryForm(strings.Repeat("x", 71), strings.Repeat("x", 71))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := captured.count()
			assertAmbiguousBody(t, serveWithContentType(s, principal, "/v1/chat/completions", tc.contentType, tc.body))
			if captured.count() != before {
				t.Fatal("ambiguous body forwarded")
			}
		})
	}
	// Boundaries common clients generate keep working, bare or quoted.
	for _, boundary := range []string{
		"0123456789abcdef0123456789abcdef",                   // httpx, requests
		"----formdata-undici-012345678901",                   // undici (fetch)
		"--------------------------123456789012345678901234", // form-data (node)
		"------------------------d74496d66958873e",           // curl
		strings.Repeat("0a1b2c3d4e", 6),                      // Go multipart.Writer
		"----WebKitFormBoundary7MA4YWxkTrZu0gW",              // browsers
		strings.Repeat("x", 70),                              // RFC 2046 maximum
		"a'()+_,-./:=?b",                                     // every bchar punctuation
	} {
		form := "--" + boundary + "\r\nContent-Disposition: form-data; name=\"model\"\r\n\r\ntest-model\r\n--" + boundary + "--\r\n"
		contentTypes := []string{`multipart/form-data; boundary="` + boundary + `"`}
		if !strings.ContainsAny(boundary, "'()+,/:=?") {
			contentTypes = append(contentTypes, "multipart/form-data; boundary="+boundary)
		}
		for _, contentType := range contentTypes {
			if w := serveWithContentType(s, principal, "/v1/chat/completions", contentType, form); w.Code != http.StatusOK {
				t.Fatalf("%s: status=%d body=%s", contentType, w.Code, w.Body.String())
			}
		}
	}
}

// utf16le encodes ASCII text as UTF-16LE bytes.
func utf16le(text string) string {
	var b strings.Builder
	for _, c := range []byte(text) {
		b.WriteByte(c)
		b.WriteByte(0)
	}
	return b.String()
}

// M2: a protected form field whose bytes other parsers decode differently is
// refused: a Content-Transfer-Encoding (mime/multipart's NextPart decodes
// quoted-printable, NextRawPart and busboy do not), a charset other than
// UTF-8/US-ASCII (busboy decodes it, the proxy reads raw bytes), or a part
// Content-Type that does not parse. File parts and other fields are free.
func TestMultipartProtectedFieldEncodingRejected(t *testing.T) {
	s, captured, principal := newNormalizeHTTPService(t, []config.ModelMapping{{Upstream: "test-model"}})
	form := func(headers, value string) string {
		return "--b\r\nContent-Disposition: form-data; name=\"prompt\"\r\n\r\nhi\r\n" +
			"--b\r\n" + headers + "\r\n\r\n" + value + "\r\n--b--\r\n"
	}
	const model = `Content-Disposition: form-data; name="model"`
	for _, tc := range []struct{ name, headers, value string }{
		{"utf-16le", model + "\r\nContent-Type: text/plain; charset=utf-16le", utf16le("gpt-secret")},
		{"latin1", model + "\r\nContent-Type: text/plain; charset=ISO-8859-1", "test-model"},
		{"quoted-printable", model + "\r\nContent-Transfer-Encoding: quoted-printable", "gpt=2Dsecret"},
		{"base64", model + "\r\nContent-Transfer-Encoding: base64", "Z3B0LXNlY3JldA=="},
		{"8bit", model + "\r\nContent-Transfer-Encoding: 8bit", "test-model"},
		{"unparseable content type", model + "\r\nContent-Type: text/plain; charset", "test-model"},
		{"two content types", model + "\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Type: text/plain; charset=utf-16le", utf16le("gpt-secret")},
		{"case variant field", `Content-Disposition: form-data; name="Stream"` + "\r\nContent-Transfer-Encoding: quoted-printable", "true"},
		{"image parameter", `Content-Disposition: form-data; name="size"` + "\r\nContent-Type: text/plain; charset=utf-16le", utf16le("1024x1024")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := captured.count()
			// An allowed model part first: only the encoding rule can refuse.
			body := "--b\r\n" + model + "\r\n\r\ntest-model\r\n" + form(tc.headers, tc.value)
			if reason := requestBodyAmbiguity("multipart/form-data; boundary=b", []byte(body)); !strings.HasPrefix(reason, "multipart/form-data protected field ") {
				t.Fatalf("reason %q, want the protected field encoding rule", reason)
			}
			assertAmbiguousBody(t, serveWithContentType(s, principal, "/v1/chat/completions", "multipart/form-data; boundary=b", body))
			if captured.count() != before {
				t.Fatal("ambiguous body forwarded")
			}
		})
	}
	for _, tc := range []struct{ name, headers, value string }{
		{"UTF-8 charset", model + "\r\nContent-Type: text/plain; charset=UTF-8", "test-model"},
		{"US-ASCII charset", model + "\r\nContent-Type: text/plain; charset=us-ascii", "test-model"},
		{"no charset", model + "\r\nContent-Type: text/plain", "test-model"},
		{"file part with content type", model + "\r\n" + "Content-Type: text/plain\r\n\r\ntest-model\r\n--b\r\n" +
			`Content-Disposition: form-data; name="image"; filename="a.png"` + "\r\nContent-Type: image/png; charset=binary\r\nContent-Transfer-Encoding: binary", "\x89PNG"},
		{"other field", model + "\r\n\r\ntest-model\r\n--b\r\n" + `Content-Disposition: form-data; name="prompt2"` +
			"\r\nContent-Type: text/plain; charset=utf-16le\r\nContent-Transfer-Encoding: 8bit", utf16le("hi")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := serveWithContentType(s, principal, "/v1/chat/completions", "multipart/form-data; boundary=b", form(tc.headers, tc.value))
			if w.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
}

// L2: a request with more than one Content-Type header and a body is refused:
// the proxy reads the first value, copyHeaders forwards both and upstreams
// pick either. A body-less request is unaffected.
func TestDuplicateContentTypeRejected(t *testing.T) {
	s, captured, principal := newNormalizeHTTPService(t, []config.ModelMapping{{Upstream: "test-model"}})
	serve := func(body string, contentTypes ...string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		for _, contentType := range contentTypes {
			r.Header.Add("Content-Type", contentType)
		}
		r = r.WithContext(auth.WithPrincipal(r.Context(), principal))
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w
	}
	for _, contentTypes := range [][]string{
		{"application/json", urlEncodedType},
		{"application/json", "application/json"},
		{urlEncodedType, "multipart/form-data; boundary=b"},
	} {
		before := captured.count()
		assertAmbiguousBody(t, serve(`{"model":"test-model","messages":[]}`, contentTypes...))
		if captured.count() != before {
			t.Fatalf("%v: ambiguous body forwarded", contentTypes)
		}
	}
	if w := serve(`{"model":"test-model","messages":[]}`, "application/json"); w.Code != http.StatusOK {
		t.Fatalf("single Content-Type: status=%d body=%s", w.Code, w.Body.String())
	}
	if got := captured.lastHeader(t).Values("Content-Type"); len(got) != 1 {
		t.Fatalf("upstream Content-Type values %v", got)
	}
}
