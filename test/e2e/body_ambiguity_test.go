//go:build e2e

package e2e

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// The declared content type and the body shape must not let the proxy read
// one model and the upstream another: a JSON object sent as urlencoded is read
// and forwarded as JSON (curl -d keeps working), and multipart bodies a JSON
// or other form parser would read differently are refused with
// ambiguous_request_body.
func TestRequestBodyAmbiguity(t *testing.T) {
	name := target(t, map[string]any{"endpoint_type": "openai", "endpoint": w.urlA, "api_key": "sk-up-amb",
		"model_mappings": []any{map[string]any{"upstream": "e2e-amb-model"}}})
	_, key := client(t, []string{name}, nil)
	urlencoded := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}
	multipartHdr := map[string]string{"Content-Type": "multipart/form-data; boundary=b"}
	form := "--b\r\nContent-Disposition: form-data; name=\"model\"\r\n\r\ne2e-amb-model\r\n--b--\r\n"

	// curl -d style: JSON with the urlencoded content type is read as JSON and
	// declared as JSON upstream (charset kept), whatever form-like text its
	// strings hold, so the upstream reads the same model.
	for _, tc := range []struct{ contentType, body, want string }{
		{"application/x-www-form-urlencoded", `{"model":"e2e-amb-model","messages":[{"role":"user","content":"a&model=gpt-secret"}]}`, "application/json"},
		{"application/x-www-form-urlencoded; charset=utf-8", `{"model":"e2e-amb-model","messages":[{"role":"user","content":"see https://x.io/?a=1&n=2"}]}`, "application/json; charset=utf-8"},
	} {
		seq := w.fakeA.LastSeq()
		resp, body := call(t, key, "POST", "/v1/chat/completions", tc.body, map[string]string{"Content-Type": tc.contentType})
		if resp.StatusCode != 200 {
			t.Fatalf("curl -d style %q: status %d: %s", tc.contentType, resp.StatusCode, body)
		}
		rec := lastNewRecord(t, w.fakeA, seq)
		upstreamMember(t, rec, "model", `"e2e-amb-model"`)
		if rec.Model != "e2e-amb-model" || rec.Headers["content-type"] != tc.want {
			t.Fatalf("curl -d style %q: upstream model %q content-type %q, want %q", tc.contentType, rec.Model, rec.Headers["content-type"], tc.want)
		}
	}
	// A plain form keeps its content type.
	seq := w.fakeA.LastSeq()
	resp, body := call(t, key, "POST", "/v1/chat/completions", "model=e2e-amb-model&messages=x", urlencoded)
	if resp.StatusCode != 200 {
		t.Fatalf("form: status %d: %s", resp.StatusCode, body)
	}
	if rec := lastNewRecord(t, w.fakeA, seq); rec.Headers["content-type"] != "application/x-www-form-urlencoded" {
		t.Fatalf("form: upstream content-type %q", rec.Headers["content-type"])
	}

	// A JSON model the client may not use is refused, whatever form-like
	// text its strings hold.
	seq = w.fakeA.LastSeq()
	resp, body = call(t, key, "POST", "/v1/chat/completions",
		`{"model":"gpt-secret","x":"&model=e2e-amb-model&y=","messages":[]}`, urlencoded)
	if resp.StatusCode < 400 || resp.StatusCode >= 500 {
		t.Fatalf("hidden form model accepted: %d %s", resp.StatusCode, body)
	}
	if recs := newRecords(w.fakeA, seq); len(recs) != 0 {
		t.Fatalf("rejected request reached upstream: %+v", recs)
	}

	// M1: urlencoded bodies that look like JSON but are not one valid JSON
	// object (lenient-JSON shapes, a BOM) are refused instead of read as a
	// form.
	for _, tc := range []struct{ name, body string }{
		{"NaN", `{"model":"gpt-secret","t":NaN,"x":"&model=e2e-amb-model&y="}`},
		{"trailing comma", `{"model":"gpt-secret","x":"&model=e2e-amb-model&y=",}`},
		{"BOM", "\xef\xbb\xbf" + `{"model":"gpt-secret","x":"&model=e2e-amb-model&y="}`},
	} {
		seq := w.fakeA.LastSeq()
		resp, body := call(t, key, "POST", "/v1/chat/completions", tc.body, urlencoded)
		var decoded struct {
			Error struct{ Code string } `json:"error"`
		}
		_ = json.Unmarshal(body, &decoded)
		if resp.StatusCode != 400 || decoded.Error.Code != "ambiguous_request_body" {
			t.Fatalf("urlencoded %s: status %d: %s", tc.name, resp.StatusCode, body)
		}
		if recs := newRecords(w.fakeA, seq); len(recs) != 0 {
			t.Fatalf("urlencoded %s: rejected request reached upstream: %+v", tc.name, recs)
		}
	}

	for _, tc := range []struct{ name, body string }{
		{"JSON before boundary", `{"model":"gpt-secret","messages":[]}` + "\r\n" + form},
		{"text preamble", "model=gpt-secret\r\n" + form},
		{"RFC 2231 name", "--b\r\nContent-Disposition: form-data; name=\"x\"; name*=utf-8''model\r\n\r\ngpt-secret\r\n" + form},
		{"duplicate name", "--b\r\nContent-Disposition: form-data; name=\"model\"; name=\"x\"\r\n\r\ngpt-secret\r\n" + form},
		// A boundary-prefixed line mime/multipart does not split at, which
		// busboy-style parsers would.
		{"stray boundary line", "--b\r\nContent-Disposition: form-data; name=\"model\"\r\n\r\ne2e-amb-model\r\n--bX\r\nContent-Disposition: form-data; name=\"model\"\r\n\r\ngpt-secret\r\n--b--\r\n"},
		{"part after closing boundary", form + form},
		// A CRLF form whose part headers end at a bare LF line: mime/multipart
		// reads n="" and model=e2e-amb-model, busboy merges the parts.
		{"bare LF header end", "--b\r\nContent-Disposition: form-data; name=\"n\"\r\n\n--b\r\nContent-Disposition: form-data; name=\"note\"\r\n\r\ngpt-secret\r\n" + form},
		// M2: a protected field in a charset busboy decodes and the proxy
		// reads as raw bytes (UTF-16LE "gpt-secret").
		{"UTF-16LE model field", "--b\r\nContent-Disposition: form-data; name=\"model\"\r\nContent-Type: text/plain; charset=utf-16le\r\n\r\n" +
			"g\x00p\x00t\x00-\x00s\x00e\x00c\x00r\x00e\x00t\x00\r\n--b--\r\n"},
		{"bare LF header end before body", "--b\r\nContent-Disposition: form-data; name=\"model\"\r\n\ne2e-amb-model\r\n--b--\r\n"},
	} {
		seq := w.fakeA.LastSeq()
		resp, body := call(t, key, "POST", "/v1/chat/completions", tc.body, multipartHdr)
		var decoded struct {
			Error struct{ Code string } `json:"error"`
		}
		_ = json.Unmarshal(body, &decoded)
		if resp.StatusCode != 400 || decoded.Error.Code != "ambiguous_request_body" {
			t.Fatalf("%s: status %d: %s", tc.name, resp.StatusCode, body)
		}
		if recs := newRecords(w.fakeA, seq); len(recs) != 0 {
			t.Fatalf("%s: rejected request reached upstream: %+v", tc.name, recs)
		}
	}
	// H1: a boundary parameter with surrounding whitespace is refused. The
	// trimmed boundary would read the first form (allowed model), the raw one
	// (mime/multipart, the upstream) the second (another model).
	for _, raw := range []string{" b", "\tb", "b ", ""} {
		trimmedPart := "--b\r\nContent-Disposition: form-data; name=\"model\"\r\n\r\ne2e-amb-model\r\n--b--\r\n"
		rawPart := "--" + raw + "\r\nContent-Disposition: form-data; name=\"model\"\r\n\r\ngpt-secret\r\n--" + raw + "--\r\n"
		seq := w.fakeA.LastSeq()
		resp, body := call(t, key, "POST", "/v1/chat/completions", trimmedPart+rawPart,
			map[string]string{"Content-Type": `multipart/form-data; boundary="` + raw + `"`})
		var decoded struct {
			Error struct{ Code string } `json:"error"`
		}
		_ = json.Unmarshal(body, &decoded)
		if resp.StatusCode != 400 || decoded.Error.Code != "ambiguous_request_body" {
			t.Fatalf("boundary %q: status %d: %s", raw, resp.StatusCode, body)
		}
		if recs := newRecords(w.fakeA, seq); len(recs) != 0 {
			t.Fatalf("boundary %q: rejected request reached upstream: %+v", raw, recs)
		}
	}
	// H2: the boundary parameter is checked as written. mime/multipart reads
	// the first form (allowed model) on its unescaped or RFC 2231 boundary;
	// busboy-style parsers read the second (another model) on theirs.
	twoForms := func(goBoundary, otherBoundary string) string {
		part := func(boundary, model string) string {
			return "--" + boundary + "\r\nContent-Disposition: form-data; name=\"model\"\r\n\r\n" + model + "\r\n--" + boundary + "--\r\n"
		}
		return part(goBoundary, "e2e-amb-model") + part(otherBoundary, "gpt-secret")
	}
	for _, tc := range []struct{ name, contentType, body string }{
		{"escaped boundary", `multipart/form-data; boundary="AaB\(x"`, twoForms("AaB(x", `AaB\(x`)},
		{"RFC 2231 boundary", `multipart/form-data; boundary=AAA; boundary*=utf-8''BBB`, twoForms("BBB", "AAA")},
		{"RFC 2231 continuation", `multipart/form-data; boundary*0=BB; boundary*1=B; boundary=AAA`, twoForms("BBB", "AAA")},
		{"not RFC 2046", `multipart/form-data; boundary="a@b"`, "--a@b\r\nContent-Disposition: form-data; name=\"model\"\r\n\r\ne2e-amb-model\r\n--a@b--\r\n"},
	} {
		seq := w.fakeA.LastSeq()
		resp, body := call(t, key, "POST", "/v1/chat/completions", tc.body, map[string]string{"Content-Type": tc.contentType})
		var decoded struct {
			Error struct{ Code string } `json:"error"`
		}
		_ = json.Unmarshal(body, &decoded)
		if resp.StatusCode != 400 || decoded.Error.Code != "ambiguous_request_body" {
			t.Fatalf("%s: status %d: %s", tc.name, resp.StatusCode, body)
		}
		if recs := newRecords(w.fakeA, seq); len(recs) != 0 {
			t.Fatalf("%s: rejected request reached upstream: %+v", tc.name, recs)
		}
	}
	// A clean multipart form still works, including client-generated
	// boundaries, bare or quoted.
	if resp, body := call(t, key, "POST", "/v1/chat/completions", "\r\n"+form, multipartHdr); resp.StatusCode != 200 {
		t.Fatalf("clean form: status %d: %s", resp.StatusCode, body)
	}
	for _, contentType := range []string{
		"multipart/form-data; boundary=----formdata-undici-012345678901",
		`multipart/form-data; boundary="------------------------d74496d66958873e"`,
	} {
		_, boundary, _ := strings.Cut(contentType, "boundary=")
		boundary = strings.Trim(boundary, `"`)
		clean := "--" + boundary + "\r\nContent-Disposition: form-data; name=\"model\"\r\n\r\ne2e-amb-model\r\n--" + boundary + "--\r\n"
		if resp, body := call(t, key, "POST", "/v1/chat/completions", clean, map[string]string{"Content-Type": contentType}); resp.StatusCode != 200 {
			t.Fatalf("%s: status %d: %s", contentType, resp.StatusCode, body)
		}
	}
}

// L2: a body with more than one Content-Type header is refused, never
// forwarded: the proxy reads the first value, an upstream may pick another.
func TestDuplicateContentTypeRejected(t *testing.T) {
	name := target(t, map[string]any{"endpoint_type": "openai", "endpoint": w.urlA, "api_key": "sk-up-dupct",
		"model_mappings": []any{map[string]any{"upstream": "e2e-dupct-model"}}})
	_, key := client(t, []string{name}, nil)
	seq := w.fakeA.LastSeq()
	req, _ := http.NewRequest("POST", w.base+"/v1/chat/completions", strings.NewReader(`{"model":"e2e-dupct-model","messages":[]}`))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Add("Content-Type", "application/json")
	req.Header.Add("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var decoded struct {
		Error struct{ Code string } `json:"error"`
	}
	_ = json.Unmarshal(body, &decoded)
	if resp.StatusCode != 400 || decoded.Error.Code != "ambiguous_request_body" {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	if recs := newRecords(w.fakeA, seq); len(recs) != 0 {
		t.Fatalf("rejected request reached upstream: %+v", recs)
	}
	// One Content-Type keeps working.
	if resp, body := call(t, key, "POST", "/v1/chat/completions", `{"model":"e2e-dupct-model","messages":[]}`, nil); resp.StatusCode != 200 {
		t.Fatalf("single Content-Type: status %d: %s", resp.StatusCode, body)
	}
}

// L1: an encoded request body is refused with 415 unsupported_content_encoding
// and never forwarded unread; identity bodies keep working.
func TestRequestContentEncodingRejected(t *testing.T) {
	name := target(t, map[string]any{"endpoint_type": "openai", "endpoint": w.urlA, "api_key": "sk-up-enc",
		"model_mappings": []any{map[string]any{"upstream": "e2e-enc-model"}}})
	_, key := client(t, []string{name}, nil)
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write([]byte(`{"model":"gpt-secret","messages":[]}`))
	_ = zw.Close()
	for _, encoding := range []string{"gzip", "identity, gzip", "br"} {
		seq := w.fakeA.LastSeq()
		resp, body := call(t, key, "POST", "/v1/chat/completions", gz.String(), map[string]string{"Content-Encoding": encoding})
		var decoded struct {
			Error struct{ Code string } `json:"error"`
		}
		_ = json.Unmarshal(body, &decoded)
		if resp.StatusCode != http.StatusUnsupportedMediaType || decoded.Error.Code != "unsupported_content_encoding" {
			t.Fatalf("%s: status %d: %s", encoding, resp.StatusCode, body)
		}
		if recs := newRecords(w.fakeA, seq); len(recs) != 0 {
			t.Fatalf("%s: rejected request reached upstream: %+v", encoding, recs)
		}
	}
	if resp, body := call(t, key, "POST", "/v1/chat/completions", `{"model":"e2e-enc-model","messages":[{"role":"user","content":"hi"}]}`,
		map[string]string{"Content-Encoding": "identity"}); resp.StatusCode != 200 {
		t.Fatalf("identity: status %d: %s", resp.StatusCode, body)
	}
}
