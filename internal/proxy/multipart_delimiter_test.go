package proxy

import (
	"bytes"
	"crypto/rand"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

// Residual hardening: a line starting with "--"+boundary that mime/multipart
// does not use as a delimiter (content after the prefix, a CRLF form's
// LF-only line, a line after the closing boundary, a bad closing line) is
// refused. Parsers such as busboy split at such lines and would read parts
// the proxy never saw.
func TestMultipartStrayBoundaryLineRejected(t *testing.T) {
	s, captured, principal := newNormalizeHTTPService(t, []config.ModelMapping{{Upstream: "test-model"}})
	const b = multipartTestBoundary
	model := func(nl, value string) string {
		return "Content-Disposition: form-data; name=\"model\"" + nl + nl + value
	}
	hidden := "Content-Disposition: form-data; name=\"model\"\r\n\r\ngpt-secret"
	for _, tc := range []struct{ name, body string }{
		{"suffix after boundary", "--" + b + "\r\n" + model("\r\n", "test-model") + "\r\n--" + b + "X\r\n" + hidden + "\r\n--" + b + "--\r\n"},
		{"dash suffix", "--" + b + "\r\n" + model("\r\n", "test-model") + "\r\n--" + b + "-\r\n" + hidden + "\r\n--" + b + "--\r\n"},
		{"space then text", "--" + b + "\r\n" + model("\r\n", "test-model") + "\r\n--" + b + " x\r\n" + hidden + "\r\n--" + b + "--\r\n"},
		{"LF-only line in CRLF form", "--" + b + "\r\n" + model("\r\n", "test-model") + "\n--" + b + "\r\n" + hidden + "\r\n--" + b + "--\r\n"},
		{"CRLF line in LF form", "--" + b + "\n" + model("\n", "test-model") + "\n--" + b + "\r\n" + hidden + "\n--" + b + "--\n"},
		{"part after closing boundary", "--" + b + "\r\n" + model("\r\n", "test-model") + "\r\n--" + b + "--\r\n--" + b + "\r\n" + hidden + "\r\n--" + b + "--\r\n"},
		{"bad closing line", "--" + b + "\r\n" + model("\r\n", "test-model") + "\r\n--" + b + "--x\r\n"},
		{"LF closing line in CRLF form", "--" + b + "\r\n" + model("\r\n", "test-model") + "\r\n--" + b + "--\n"},
		{"boundary-prefixed header", "--" + b + "\r\n--" + b + ": x\r\n" + model("\r\n", "test-model") + "\r\n--" + b + "--\r\n"},
		// A CRLF form whose part headers end at a bare LF line: mime/multipart
		// ends the headers there (and accepts a delimiter right after them),
		// busboy does not and merges the parts (n=gpt-secret).
		{"bare LF header end before delimiter", "--" + b + "\r\nContent-Disposition: form-data; name=\"n\"\r\n\n--" + b + "\r\nContent-Disposition: form-data; name=\"note\"\r\n\r\ngpt-secret\r\n--" + b + "\r\n" + model("\r\n", "test-model") + "\r\n--" + b + "--\r\n"},
		{"bare LF header end", "--" + b + "\r\nContent-Disposition: form-data; name=\"model\"\r\n\ntest-model\r\n--" + b + "--\r\n"},
		{"bare LF header end in second part", "--" + b + "\r\n" + model("\r\n", "test-model") + "\r\n--" + b + "\r\nContent-Disposition: form-data; name=\"x\"\n\ny\r\n--" + b + "--\r\n"},
		{"bare LF empty header block", "--" + b + "\r\n\n--" + b + "\r\n" + model("\r\n", "test-model") + "\r\n--" + b + "--\r\n"},
		{"bare LF header line", "--" + b + "\r\nContent-Disposition: form-data; name=\"model\"\n\r\ntest-model\r\n--" + b + "--\r\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := captured.count()
			assertAmbiguousBody(t, serveWithContentType(s, principal, "/v1/chat/completions", "multipart/form-data; boundary="+b, tc.body))
			if captured.count() != before {
				t.Fatal("ambiguous body forwarded")
			}
		})
	}
	// Valid delimiter lines keep working: transport padding, LF forms, a
	// closing line at the end of the body, epilogue text, and boundary-like
	// text that does not start a line.
	for _, body := range []string{
		"--" + b + " \t\r\n" + model("\r\n", "test-model") + "\r\n--" + b + "-- \r\n",
		"--" + b + "\n" + model("\n", "test-model") + "\n--" + b + "--",
		"--" + b + "\r\n" + model("\r\n", "test-model") + "\r\n--" + b + "--\r\nepilogue\r\n",
		"--" + b + "\r\n" + model("\r\n", "test-model") + "\r\n--" + b + "\r\n" + `Content-Disposition: form-data; name="prompt"` + "\r\n\r\nsee x--" + b + "X here\r\n--" + b + "--\r\n",
		// Bare LF lines in a CRLF form's part body are content.
		"--" + b + "\r\n" + model("\r\n", "test-model") + "\r\n--" + b + "\r\n" + `Content-Disposition: form-data; name="prompt"` + "\r\n\r\n\na\n\nb\n\r\n--" + b + "--\r\n",
	} {
		if w := serveWithContentType(s, principal, "/v1/chat/completions", "multipart/form-data; boundary="+b, body); w.Code != http.StatusOK {
			t.Fatalf("%q: status=%d body=%s", body, w.Code, w.Body.String())
		}
	}
}

// lineStartBoundaries counts lines that start with "--"+boundary.
func lineStartBoundaries(body []byte, boundary string) int {
	dash := []byte("--" + boundary)
	count := 0
	if bytes.HasPrefix(body, dash) {
		count++
	}
	return count + bytes.Count(body, append([]byte("\n"), dash...))
}

// goMultipartPartCount counts the parts mime/multipart reads when it ends at
// the closing boundary: NextPart returns io.EOF itself, not an error that
// wraps io.EOF (a body truncated after a bare delimiter line).
func goMultipartPartCount(body []byte, boundary string) (int, bool) {
	reader := multipart.NewReader(bytes.NewReader(body), boundary)
	for parts := 0; ; parts++ {
		part, err := reader.NextPart()
		if err == io.EOF {
			return parts, true
		}
		if err != nil {
			return 0, false
		}
		if _, err := io.Copy(io.Discard, part); err != nil {
			return 0, false
		}
	}
}

// crlfHeaderBlockNotCRLF is the oracle for the header rule, by a plain line
// walk: in a CRLF form (its first delimiter line ends in CRLF) some part's
// header block, up to and including the empty line that ends it, has a line
// not ending in CRLF. Every line starting with "--"+boundary is taken as a
// delimiter, which holds when mime/multipart used all of them.
func crlfHeaderBlockNotCRLF(body []byte, boundary string) bool {
	dash := []byte("--" + boundary)
	crlf, inHeaders, seen := false, false, false
	for len(body) > 0 {
		end := bytes.IndexByte(body, '\n') + 1
		if end == 0 {
			end = len(body)
		}
		line := body[:end]
		body = body[end:]
		if bytes.HasPrefix(line, dash) {
			rest := line[len(dash):]
			if bytes.HasPrefix(rest, []byte("--")) {
				return false // the closing line: no part after it
			}
			if !seen {
				crlf = bytes.HasSuffix(line, []byte("\r\n"))
				seen = true
			}
			inHeaders = true
			continue
		}
		if !inHeaders || !crlf || !bytes.HasSuffix(line, []byte("\n")) {
			continue // a last line without a line break ends the body
		}
		if !bytes.HasSuffix(line, []byte("\r\n")) {
			return true
		}
		if len(line) == 2 {
			inHeaders = false
		}
	}
	return false
}

// checkMultipartStrayDelimiter compares the scan with mime/multipart: for a
// form Go reads completely after a whitespace preamble, the body is clean
// exactly when Go used every boundary-prefixed line (each part's delimiter
// plus the closing line) and, in a CRLF form, every part's header block is
// CRLF only (mime/multipart ends headers at a bare LF line, busboy does not).
func checkMultipartStrayDelimiter(t *testing.T, body []byte, boundary string) {
	t.Helper()
	stray := multipartStrayDelimiter(body, boundary)
	end := multipartPreambleEnd(body, boundary)
	if end == len(body) || len(bytes.Trim(body[:end], " \t\r\n")) > 0 {
		return // refused or not a form before this check
	}
	parts, ok := goMultipartPartCount(body, boundary)
	if !ok {
		return
	}
	used := parts + 1
	got := lineStartBoundaries(body, boundary)
	want := got != used || crlfHeaderBlockNotCRLF(body, boundary)
	if stray != want {
		t.Fatalf("%q: stray=%v, want %v (%d boundary lines, mime/multipart used %d)", body, stray, want, got, used)
	}
}

var strayDelimiterCorpus = []string{
	"--b\r\nContent-Disposition: form-data; name=\"model\"\r\n\r\nm\r\n--b--\r\n",
	"--b\r\nContent-Disposition: form-data; name=\"model\"\r\n\r\nm\r\n--bX\r\nx\r\n--b--\r\n",
	"--b\nContent-Disposition: form-data; name=\"model\"\n\nm\n--b\r\n\n--b--",
	"--b\r\n\r\nm\r\n--b--\r\n--b\r\n\r\nx\r\n--b--\r\n",
	"--b-- \r\n",
	"\r\n--b\t\r\n\r\nm\n--b\r\n\r\n--b--x",
	"--b\r\n--b: x\r\n\r\nm\r\n--b--\r\n",
	"--b  \n\n--b",
	"--b\r\n\n--b--",
	"--b\r\nX: y\r\n\n--b--\r\n",
	"--b\r\nX: y\r\n\r\nm\n\n--b--\r\n",
	"--b\r\nX: y\r\n\nm\r\n--b--\r\n",
	"--b\r\nX: y\n\r\nm\r\n--b--\r\n",
	"--b\r\nContent-Disposition: form-data; name=\"n\"\r\n\n--b\r\nContent-Disposition: form-data; name=\"note\"\r\n\r\n10\r\n--b--\r\n",
	"--b\r\nX: y\r\n\r\nm\r\n--b\r\nZ: w\r\n\n\r\n--b--\r\n",
	"--b\nX: y\n\nm\n--b--\n",
	"--b\r\n0:",
}

func FuzzMultipartStrayDelimiter(f *testing.F) {
	for _, body := range strayDelimiterCorpus {
		f.Add([]byte(body))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		checkMultipartStrayDelimiter(t, body, "b")
	})
}

func TestMultipartStrayDelimiterMatchesMime(t *testing.T) {
	for _, body := range strayDelimiterCorpus {
		checkMultipartStrayDelimiter(t, []byte(body), "b")
	}
}

// The scan is linear (bytes.Index over the body) on large uploads.
func BenchmarkMultipartStrayDelimiter(b *testing.B) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	_ = writer.WriteField("model", "test-model")
	file, _ := writer.CreateFormFile("image", "a.png")
	payload := make([]byte, 32<<20)
	_, _ = rand.Read(payload)
	_, _ = file.Write(payload)
	_ = writer.Close()
	contentType := writer.FormDataContentType()
	boundary := strings.TrimPrefix(contentType, "multipart/form-data; boundary=")
	data := body.Bytes()
	b.Run("scan", func(b *testing.B) {
		b.SetBytes(int64(len(data)))
		for range b.N {
			if multipartStrayDelimiter(data, boundary) {
				b.Fatal("clean upload refused")
			}
		}
	})
	b.Run("requestBodyAmbiguity", func(b *testing.B) {
		b.SetBytes(int64(len(data)))
		for range b.N {
			if reason := requestBodyAmbiguity(contentType, data); reason != "" {
				b.Fatal(reason)
			}
		}
	})
}
