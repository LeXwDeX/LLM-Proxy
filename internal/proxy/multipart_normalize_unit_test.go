package proxy

import (
	"bytes"
	"errors"
	"io"
	"mime/multipart"
	"strconv"
	"strings"
	"testing"
)

func rawMultipart(nl string, parts ...string) []byte {
	var b strings.Builder
	for _, part := range parts {
		b.WriteString("--" + multipartTestBoundary + nl + part + nl)
	}
	b.WriteString("--" + multipartTestBoundary + "--" + nl)
	return []byte(b.String())
}

func textPart(nl, name, value string) string {
	return `Content-Disposition: form-data; name="` + name + `"` + nl + nl + value
}

func TestNormalizeMultipartMembersRules(t *testing.T) {
	crlf := "\r\n"
	image := `Content-Disposition: form-data; name="image"; filename="a.png"` + crlf + "Content-Type: image/png" + crlf + crlf + "\x89PNG\r\n--not-a-boundary\r\n"
	for _, tc := range []struct {
		name       string
		body, want []byte
		report     string
	}{
		{"no change keeps bytes", rawMultipart(crlf, textPart(crlf, "model", "m"), image), nil, ""},
		{"unprotected duplicates untouched", rawMultipart(crlf, textPart(crlf, "tag", "a"), textPart(crlf, "Tag", "b"), textPart(crlf, "tag", "c")), nil, ""},
		{"last exact wins",
			rawMultipart(crlf, textPart(crlf, "model", "gpt-secret"), image, textPart(crlf, "model", "m")),
			rawMultipart(crlf, image, textPart(crlf, "model", "m")), "model:1"},
		{"variants removed",
			rawMultipart(crlf, textPart(crlf, "Model", "x"), textPart(crlf, "model", "m"), textPart(crlf, "MODEL", "y"), textPart(crlf, "n", "2")),
			rawMultipart(crlf, textPart(crlf, "model", "m"), textPart(crlf, "n", "2")), "model:2"},
		{"variant only removed",
			rawMultipart(crlf, image, textPart(crlf, "Model", "secret")),
			rawMultipart(crlf, image), "model:1"},
		{"last part removed",
			rawMultipart(crlf, textPart(crlf, "size", "1024x1024"), textPart(crlf, "SIZE", "4096x4096")),
			rawMultipart(crlf, textPart(crlf, "size", "1024x1024")), "size:1"},
		{"only part removed",
			rawMultipart(crlf, textPart(crlf, "Quality", "high")),
			[]byte("--" + multipartTestBoundary + "--" + crlf), "quality:1"},
		{"LF line endings",
			rawMultipart("\n", textPart("\n", "model", "a"), textPart("\n", "model", "b")),
			rawMultipart("\n", textPart("\n", "model", "b")), "model:1"},
		{"preamble padding and epilogue",
			[]byte("preamble line\r\n--" + multipartTestBoundary + " \t\r\n" + textPart(crlf, "n", "1") + "\r\n--" + multipartTestBoundary + "\r\n" + textPart(crlf, "n", "3") + "\r\n--" + multipartTestBoundary + "--\r\nepilogue"),
			[]byte("preamble line\r\n--" + multipartTestBoundary + "\r\n" + textPart(crlf, "n", "3") + "\r\n--" + multipartTestBoundary + "--\r\nepilogue"), "n:1"},
		{"unparseable untouched", []byte("--" + multipartTestBoundary + "\r\n" + textPart(crlf, "model", "a") + "\r\n--" + multipartTestBoundary + "\r\n" + textPart(crlf, "model", "b")), nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, report := normalizeMultipartMembers(tc.body, multipartTestBoundary)
			want := tc.want
			if want == nil {
				want = tc.body
				if len(got) > 0 && &got[0] != &tc.body[0] {
					t.Fatal("a body needing no change was copied")
				}
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("got  %q\nwant %q", got, want)
			}
			var parts []string
			for _, entry := range report {
				parts = append(parts, entry.Member+":"+strconv.Itoa(entry.Removed))
			}
			if strings.Join(parts, ",") != tc.report {
				t.Fatalf("report %v, want %s", parts, tc.report)
			}
		})
	}
}

// A form without its closing boundary that mime/multipart still reads is
// rebuilt rather than forwarded with duplicate protected fields.
func TestNormalizeMultipartWithoutClosingBoundaryRebuilds(t *testing.T) {
	crlf := "\r\n"
	body := []byte("--" + multipartTestBoundary + crlf + textPart(crlf, "model", "gpt-secret") + crlf +
		"--" + multipartTestBoundary + crlf + textPart(crlf, "prompt", "p") + crlf +
		"--" + multipartTestBoundary + crlf + textPart(crlf, "model", "m") + crlf +
		"--" + multipartTestBoundary + crlf)
	if _, ok := splitMultipartSegments(body, multipartTestBoundary); ok {
		t.Fatal("expected the byte splitter to reject a missing closing boundary")
	}
	got, report := normalizeMultipartMembers(body, multipartTestBoundary)
	if len(report) != 1 || report[0].Member != "model" || report[0].Removed != 1 {
		t.Fatalf("report %+v", report)
	}
	names, values, ok := goMultipartParts(got, multipartTestBoundary)
	if !ok || strings.Join(names, ",") != "prompt,model" || strings.Join(values, ",") != "p,m" {
		t.Fatalf("rebuilt form %v %v %v: %q", names, values, ok, got)
	}
	if extractMultipartModel(got, multipartTestBoundary) != "m" || extractMultipartModel(body, multipartTestBoundary) != "m" {
		t.Fatal("proxy model changed by the rebuild")
	}
}

func TestExtractMultipartModelUsesLastExactField(t *testing.T) {
	crlf := "\r\n"
	for _, tc := range []struct {
		body []byte
		want string
	}{
		{rawMultipart(crlf, textPart(crlf, "model", "a"), textPart(crlf, "model", " b ")), "b"},
		{rawMultipart(crlf, textPart(crlf, "Model", "a")), ""},
		{rawMultipart(crlf, textPart(crlf, "model", "a"), textPart(crlf, "MODEL", "b")), "a"},
		// A dangling final boundary line ends the form for mime/multipart.
		{[]byte("--" + multipartTestBoundary + "\r\n" + textPart(crlf, "model", "a") + "\r\n--" + multipartTestBoundary + "\r\n"), "a"},
		// A part cut short makes the whole form unusable: no model.
		{[]byte("--" + multipartTestBoundary + "\r\n" + textPart(crlf, "model", "a") + "\r\n--" + multipartTestBoundary + "\r\n" + textPart(crlf, "n", "2")), ""},
	} {
		if got := extractMultipartModel(tc.body, multipartTestBoundary); got != tc.want {
			t.Fatalf("%q: model %q, want %q", tc.body, got, tc.want)
		}
	}
}

// goMultipartParts is the oracle: what mime/multipart reads, in order.
func goMultipartParts(body []byte, boundary string) (names, values []string, ok bool) {
	reader := multipart.NewReader(bytes.NewReader(body), boundary)
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			return names, values, true
		}
		if err != nil {
			return nil, nil, false
		}
		value, err := io.ReadAll(part)
		if err != nil {
			return nil, nil, false
		}
		names = append(names, part.FormName())
		values = append(values, string(value))
	}
}

var multipartFuzzCorpus = [][]byte{
	rawMultipart("\r\n", textPart("\r\n", "model", "a"), textPart("\r\n", "Model", "b"), textPart("\r\n", "model", "c")),
	rawMultipart("\n", textPart("\n", "n", "1"), textPart("\n", "N", "2")),
	[]byte("pre\r\n--" + multipartTestBoundary + "  \r\n" + textPart("\r\n", "size", "s") + "\r\n--" + multipartTestBoundary + "--"),
	rawMultipart("\r\n", `Content-Disposition: form-data; name="image"; filename="x"`+"\r\n\r\n"+"--"+multipartTestBoundary+"x\r\n--"+multipartTestBoundary+"-y", textPart("\r\n", "model", "m")),
	[]byte("--" + multipartTestBoundary + "--\r\n"),
}

// The byte splitter must find exactly the parts mime/multipart reads, and
// normalization must leave one exact reading of every protected field equal
// to the value the proxy used, with the other parts unchanged.
func checkMultipartNormalization(t *testing.T, body []byte) {
	t.Helper()
	names, values, ok := goMultipartParts(body, multipartTestBoundary)
	normalized, report := normalizeMultipartMembers(body, multipartTestBoundary)
	if !ok {
		if !bytes.Equal(normalized, body) {
			t.Fatalf("unparseable body changed: %q", body)
		}
		return
	}
	afterNames, afterValues, afterOK := goMultipartParts(normalized, multipartTestBoundary)
	if !afterOK {
		t.Fatalf("normalization broke %q into %q", body, normalized)
	}
	if report == nil {
		if !bytes.Equal(normalized, body) {
			t.Fatalf("no report but body changed: %q", body)
		}
		return
	}
	lastExact := func(names, values []string, field string) (string, bool) {
		value, found := "", false
		for i, name := range names {
			if name == field {
				value, found = values[i], true
			}
		}
		return value, found
	}
	for _, field := range protectedFormFields {
		folded := 0
		for _, name := range afterNames {
			if strings.EqualFold(name, field) {
				folded++
				if name != field {
					t.Fatalf("%q: variant %q survived", normalized, name)
				}
			}
		}
		if folded > 1 {
			t.Fatalf("%q: %d parts for %s survived", normalized, folded, field)
		}
		before, beforeOK := lastExact(names, values, field)
		after, afterOK := lastExact(afterNames, afterValues, field)
		if before != after || beforeOK != afterOK {
			t.Fatalf("%q: %s changed from proxy value %q to %q", body, field, before, after)
		}
	}
	var keptBefore, keptAfter []string
	for i, name := range names {
		if protectedFormFieldIndex(name) < 0 {
			keptBefore = append(keptBefore, name+"="+values[i])
		}
	}
	for i, name := range afterNames {
		if protectedFormFieldIndex(name) < 0 {
			keptAfter = append(keptAfter, name+"="+afterValues[i])
		}
	}
	if strings.Join(keptBefore, "\x00") != strings.Join(keptAfter, "\x00") {
		t.Fatalf("%q: unprotected parts changed", body)
	}
	if again, report := normalizeMultipartMembers(normalized, multipartTestBoundary); report != nil || !bytes.Equal(again, normalized) {
		t.Fatalf("not idempotent on %q", normalized)
	}
}

func TestMultipartNormalizationProperties(t *testing.T) {
	for _, body := range multipartFuzzCorpus {
		checkMultipartNormalization(t, body)
	}
}

func FuzzMultipartNormalization(f *testing.F) {
	for _, body := range multipartFuzzCorpus {
		f.Add(body)
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		checkMultipartNormalization(t, body)
	})
}
