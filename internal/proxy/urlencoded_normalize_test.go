package proxy

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

const urlEncodedContentType = "application/x-www-form-urlencoded"

// urlEncodedReadings returns how four kinds of form parsers read name: first
// or last pair, matching the decoded key exactly or case-insensitively.
func urlEncodedReadings(t *testing.T, body []byte, name string) (firstExact, lastExact, firstFold, lastFold string, folded int) {
	t.Helper()
	seenExact, seenFold := false, false
	for _, pair := range strings.Split(string(body), "&") {
		if pair == "" {
			continue
		}
		rawKey, rawValue, _ := strings.Cut(pair, "=")
		key, err := url.QueryUnescape(rawKey)
		if err != nil {
			t.Fatalf("upstream form unreadable: %q", body)
		}
		value, err := url.QueryUnescape(rawValue)
		if err != nil {
			t.Fatalf("upstream form unreadable: %q", body)
		}
		if strings.EqualFold(key, name) {
			folded++
			if !seenFold {
				firstFold, seenFold = value, true
			}
			lastFold = value
		}
		if key == name {
			if !seenExact {
				firstExact, seenExact = value, true
			}
			lastExact = value
		}
	}
	return
}

func assertSingleURLEncodedField(t *testing.T, body []byte, name, want string) {
	t.Helper()
	firstExact, lastExact, firstFold, lastFold, folded := urlEncodedReadings(t, body, name)
	if folded > 1 || firstExact != lastExact || firstExact != firstFold || firstExact != lastFold {
		t.Fatalf("form field %q is ambiguous upstream in %q: first=%q last=%q first-fold=%q last-fold=%q pairs=%d",
			name, body, firstExact, lastExact, firstFold, lastFold, folded)
	}
	if lastExact != want {
		t.Fatalf("upstream form field %q = %q, want %q (%q)", name, lastExact, want, body)
	}
}

func serveURLEncoded(s *Service, principal *auth.Principal, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	r.Header.Set("Content-Type", urlEncodedContentType)
	r = r.WithContext(auth.WithPrincipal(r.Context(), principal))
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

var urlEncodedBypassProbes = []struct {
	name, body string
	n          string // upstream n every parser must read ("" = absent)
}{
	{"duplicate model secret first", "model=gpt-secret&messages=hi&model=test-model", ""},
	{"capitalized variant last", "model=test-model&Model=gpt-secret", ""},
	{"upper variant first", "MODEL=gpt-secret&model=test-model", ""},
	{"percent-encoded variant", "model=test-model&Mod%65l=gpt-secret", ""},
	{"percent-encoded exact duplicate", "mod%65l=gpt-secret&model=test-model", ""},
	{"duplicate n", "model=test-model&n=1&N=9&n=2", "2"},
}

// A urlencoded body is forwarded as is: the proxy authorizes the last exact
// model pair and every upstream form parser must read only that value.
func TestURLEncodedMembersUnambiguousUpstream(t *testing.T) {
	for _, probe := range urlEncodedBypassProbes {
		t.Run(probe.name, func(t *testing.T) {
			s, captured, principal := newMultipartService(t, config.EndpointTypeOpenAI)
			if w := serveURLEncoded(s, principal, "/v1/chat/completions", probe.body); w.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			sent, contentType := captured.last(t)
			if contentType != urlEncodedContentType {
				t.Fatalf("content type changed: %s", contentType)
			}
			assertSingleURLEncodedField(t, sent, "model", "test-model")
			assertSingleURLEncodedField(t, sent, "n", probe.n)
		})
	}
}

// ACL reads the same pair upstream would: an unauthorized model in the last
// exact pair, or only a case variant, is refused and never forwarded.
func TestURLEncodedUnauthorizedModelCannotRideAlong(t *testing.T) {
	s, captured, principal := newMultipartService(t, config.EndpointTypeOpenAI)
	for _, body := range []string{
		"model=test-model&model=gpt-secret",
		"Model=test-model&model=gpt-secret",
		"model=test-model&mod%65l=gpt-secret",
		"Model=test-model",
	} {
		before := captured.count()
		w := serveURLEncoded(s, principal, "/v1/chat/completions", body)
		if w.Code < 400 || w.Code >= 500 || captured.count() != before {
			t.Fatalf("%q: status=%d forwarded=%v", body, w.Code, captured.count() != before)
		}
	}
}

func TestNormalizeURLEncodedMembersRules(t *testing.T) {
	for _, tc := range []struct {
		name, body, want, report string
	}{
		{"no change keeps bytes", "model=m&input=a%20b+c&x=%2F", "", ""},
		{"unprotected duplicates untouched", "tag=a&Tag=b&tag=c", "", ""},
		{"empty and trailing pairs kept", "&model=m&&x=1&", "", ""},
		{"last exact wins", "model=gpt-secret&messages=%5B%5D&model=m", "messages=%5B%5D&model=m", "model:1"},
		{"variants removed", "Model=x&model=m&MODEL=y&n=2", "model=m&n=2", "model:2"},
		{"variant only removed", "input=hi&Model=secret", "input=hi", "model:1"},
		{"encoded key decoded", "mod%65l=a&x=1&model=b", "x=1&model=b", "model:1"},
		{"encoded key kept when last", "model=a&mod%65l=b", "mod%65l=b", "model:1"},
		{"plus in key decoded", "service+tier=x&service_tier=auto&SERVICE_TIER=flex", "service+tier=x&service_tier=auto", "service_tier:1"},
		{"key without value", "stream&stream=true&n", "stream=true&n", "stream:1"},
		{"only pair removed", "Quality=high", "", "quality:1"},
		{"empty pairs around removal kept", "&model=a&&model=b&", "&&model=b&", "model:1"},
		{"image fields", "size=1024x1024&Size=4096x4096&response_format=url", "size=1024x1024&response_format=url", "size:1"},
		{"unparseable untouched", "model=a&model=b&x=%zz", "", ""},
		{"semicolon untouched", "model=a;model=b&model=c", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(tc.body)
			got, report := normalizeURLEncodedMembers(body)
			want := tc.want
			if tc.report == "" {
				want = tc.body
				if len(got) > 0 && &got[0] != &body[0] {
					t.Fatal("a body needing no change was copied")
				}
			}
			if string(got) != want {
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

func TestExtractModelURLEncodedUsesLastExactField(t *testing.T) {
	for _, tc := range []struct{ body, want string }{
		{"model=a&model=+b+", "b"},
		{"Model=a", ""},
		{"model=a&MODEL=b", "a"},
		{"mod%65l=a&model=b&mod%65l=c", "c"},
		{"model=a&model=b;x", ""},
	} {
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(tc.body))
		r.Header.Set("Content-Type", urlEncodedContentType+"; charset=utf-8")
		if got := extractModel(r, []byte(tc.body)); got != tc.want {
			t.Fatalf("%q: model %q, want %q", tc.body, got, tc.want)
		}
	}
}

var urlEncodedFuzzCorpus = []string{
	"model=a&Model=b&model=c",
	"n=1&N=2&&n=%33&",
	"mod%65l=a&model=b+c&x=%2F%2f",
	"stream&STREAM=true&size=1&Size=2",
	"model=a;b&model=c",
	"",
}

// Normalization must leave one exact reading of every protected field equal
// to the value the proxy used (the last exact pair), with the unprotected
// pairs byte for byte unchanged and in order.
func checkURLEncodedNormalization(t *testing.T, body []byte) {
	t.Helper()
	before, err := url.ParseQuery(string(body))
	normalized, report := normalizeURLEncodedMembers(body)
	if err != nil || report == nil {
		if !bytes.Equal(normalized, body) {
			t.Fatalf("body changed without a report: %q -> %q", body, normalized)
		}
		return
	}
	after, err := url.ParseQuery(string(normalized))
	if err != nil {
		t.Fatalf("normalization broke %q into %q", body, normalized)
	}
	for _, field := range protectedFormFields {
		folded := 0
		for key := range after {
			if strings.EqualFold(key, field) {
				folded += len(after[key])
				if key != field {
					t.Fatalf("%q: variant %q survived", normalized, key)
				}
			}
		}
		if folded > 1 {
			t.Fatalf("%q: %d pairs for %s survived", normalized, folded, field)
		}
		values := before[field]
		if len(values) == 0 {
			if folded != 0 {
				t.Fatalf("%q: %s appeared", normalized, field)
			}
			continue
		}
		if after.Get(field) != values[len(values)-1] {
			t.Fatalf("%q: %s changed from proxy value %q to %q", body, field, values[len(values)-1], after.Get(field))
		}
	}
	unprotected := func(raw []byte) []string {
		var kept []string
		for _, pair := range strings.Split(string(raw), "&") {
			key, _, _ := strings.Cut(pair, "=")
			if decoded, err := url.QueryUnescape(key); err != nil || protectedFormFieldIndex(decoded) < 0 {
				kept = append(kept, pair)
			}
		}
		return kept
	}
	if strings.Join(unprotected(body), "&") != strings.Join(unprotected(normalized), "&") {
		t.Fatalf("%q: unprotected pairs changed in %q", body, normalized)
	}
	if again, report := normalizeURLEncodedMembers(normalized); report != nil || !bytes.Equal(again, normalized) {
		t.Fatalf("not idempotent on %q", normalized)
	}
}

func TestURLEncodedNormalizationProperties(t *testing.T) {
	for _, body := range urlEncodedFuzzCorpus {
		checkURLEncodedNormalization(t, []byte(body))
	}
}

func FuzzURLEncodedNormalization(f *testing.F) {
	for _, body := range urlEncodedFuzzCorpus {
		f.Add([]byte(body))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		checkURLEncodedNormalization(t, body)
	})
}
