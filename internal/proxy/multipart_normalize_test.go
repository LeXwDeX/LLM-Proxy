package proxy

import (
	"bytes"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

const multipartTestBoundary = "proxy-test-boundary"

type formPart struct{ name, value, filename string }

// buildMultipart writes parts with a fixed boundary, preserving order and
// duplicates exactly as given.
func buildMultipart(t *testing.T, parts ...formPart) []byte {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.SetBoundary(multipartTestBoundary); err != nil {
		t.Fatal(err)
	}
	for _, part := range parts {
		var w io.Writer
		var err error
		if part.filename != "" {
			w, err = writer.CreateFormFile(part.name, part.filename)
		} else {
			w, err = writer.CreateFormField(part.name)
		}
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(part.value))
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return body.Bytes()
}

// multipartReadings returns how four kinds of form parsers read name: first
// or last part, matching the field name exactly or case-insensitively.
func multipartReadings(t *testing.T, body []byte, boundary, name string) (firstExact, lastExact, firstFold, lastFold string, folded int) {
	t.Helper()
	reader := multipart.NewReader(bytes.NewReader(body), boundary)
	seenExact, seenFold := false, false
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			t.Fatalf("upstream multipart unreadable: %v\n%s", err, body)
		}
		value, _ := io.ReadAll(part)
		form := part.FormName()
		if strings.EqualFold(form, name) {
			folded++
			if !seenFold {
				firstFold, seenFold = string(value), true
			}
			lastFold = string(value)
		}
		if form == name {
			if !seenExact {
				firstExact, seenExact = string(value), true
			}
			lastExact = string(value)
		}
	}
}

// assertSingleFormField fails unless every parser kind reads the same value
// and at most one part carries the (case-folded) name.
func assertSingleFormField(t *testing.T, body []byte, boundary, name, want string) {
	t.Helper()
	firstExact, lastExact, firstFold, lastFold, folded := multipartReadings(t, body, boundary, name)
	if folded > 1 || firstExact != lastExact || firstExact != firstFold || firstExact != lastFold {
		t.Fatalf("form field %q is ambiguous upstream: first=%q last=%q first-fold=%q last-fold=%q parts=%d",
			name, firstExact, lastExact, firstFold, lastFold, folded)
	}
	if lastExact != want {
		t.Fatalf("upstream form field %q = %q, want %q", name, lastExact, want)
	}
}

type capturedMultipartUpstream struct {
	mu           sync.Mutex
	bodies       [][]byte
	contentTypes []string
}

func (c *capturedMultipartUpstream) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.bodies)
}

func (c *capturedMultipartUpstream) last(t *testing.T) ([]byte, string) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.bodies) == 0 {
		t.Fatal("upstream received no request")
	}
	return c.bodies[len(c.bodies)-1], c.contentTypes[len(c.contentTypes)-1]
}

func newMultipartService(t *testing.T, endpointType string) (*Service, *capturedMultipartUpstream, *auth.Principal) {
	t.Helper()
	captured := &capturedMultipartUpstream{}
	target := config.Target{Name: "t", EndpointType: endpointType, Endpoint: "http://upstream.invalid", APIKey: "fixture",
		UsageAdapter: "standard", ModelMappings: []config.ModelMapping{{Upstream: "test-model"}}}
	if endpointType == config.EndpointTypeOpenAIImage {
		target.ImageOperation = config.ImageOperationEdits
	}
	s, err := NewService(&config.Config{
		Server:  config.ServerConfig{Bind: "127.0.0.1:0", RequestTimeoutSeconds: 5},
		Targets: []config.Target{target},
	}, newTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	s.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(r.Body)
		captured.mu.Lock()
		captured.bodies = append(captured.bodies, body)
		captured.contentTypes = append(captured.contentTypes, r.Header.Get("Content-Type"))
		captured.mu.Unlock()
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"created":1,"data":[]}`)), Request: r}, nil
	})}
	return s, captured, newAllowAllPrincipal(t, "fixture-multipart")
}

func serveMultipart(s *Service, principal *auth.Principal, path string, body []byte) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	r.Header.Set("Content-Type", "multipart/form-data; boundary="+multipartTestBoundary)
	r = r.WithContext(auth.WithPrincipal(r.Context(), principal))
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

var multipartBypassProbes = []struct {
	name  string
	parts []formPart
	n     string // upstream n every parser must read ("" = absent)
}{
	{"duplicate model secret first", []formPart{{"model", "gpt-secret", ""}, {"prompt", "p", ""}, {"model", "test-model", ""}}, ""},
	{"capitalized variant last", []formPart{{"model", "test-model", ""}, {"Model", "gpt-secret", ""}}, ""},
	{"upper variant first", []formPart{{"MODEL", "gpt-secret", ""}, {"model", "test-model", ""}}, ""},
	{"duplicate n", []formPart{{"model", "test-model", ""}, {"n", "1", ""}, {"N", "9", ""}, {"n", "2", ""}}, "2"},
}

// Multipart forwarded as is (PreserveMultipart): the proxy authorizes the last
// exact model field; every upstream form parser must read only that value.
func TestMultipartMembersUnambiguousUpstreamPassthrough(t *testing.T) {
	for _, probe := range multipartBypassProbes {
		t.Run(probe.name, func(t *testing.T) {
			s, captured, principal := newMultipartService(t, config.EndpointTypeOpenAIImage)
			parts := append([]formPart{{"image", "\x89PNG-bytes", "a.png"}}, probe.parts...)
			if w := serveMultipart(s, principal, "/v1/images/edits", buildMultipart(t, parts...)); w.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			sent, contentType := captured.last(t)
			_, params, _ := mime.ParseMediaType(contentType)
			if params["boundary"] != multipartTestBoundary {
				t.Fatalf("boundary changed: %s", contentType)
			}
			assertSingleFormField(t, sent, multipartTestBoundary, "model", "test-model")
			assertSingleFormField(t, sent, multipartTestBoundary, "n", probe.n)
			if _, _, _, image, _ := multipartReadings(t, sent, multipartTestBoundary, "image"); image != "\x89PNG-bytes" {
				t.Fatalf("file part changed: %q", image)
			}
		})
	}
}

// Multipart converted to JSON: the JSON must hold only the authorized value
// and no case variant of a protected member.
func TestMultipartMembersUnambiguousUpstreamConverted(t *testing.T) {
	for _, probe := range multipartBypassProbes {
		t.Run(probe.name, func(t *testing.T) {
			s, captured, principal := newMultipartService(t, config.EndpointTypeOpenAI)
			parts := append([]formPart{{"image", "png", "a.png"}}, probe.parts...)
			if w := serveMultipart(s, principal, "/v1/images/edits", buildMultipart(t, parts...)); w.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			sent, contentType := captured.last(t)
			if !strings.HasPrefix(contentType, "application/json") {
				t.Fatalf("not converted: %s", contentType)
			}
			assertProtectedMembersUnambiguous(t, sent)
			members, ok := scanJSONObject(nil, sent)
			if !ok {
				t.Fatalf("invalid JSON upstream: %s", sent)
			}
			for _, m := range members {
				if p := protectedMemberIndex(m); p >= 0 && !m.named(protectedRequestMembers[p]) {
					t.Fatalf("case variant %s reached upstream: %s", m.key, sent)
				}
			}
			if got := assertSingleReading(t, sent, "model"); got.value != `"test-model"` {
				t.Fatalf("upstream model %q", got.value)
			}
			want := ""
			if probe.n != "" {
				want = probe.n
			}
			if got := assertSingleReading(t, sent, "n"); got.value != want {
				t.Fatalf("upstream n %q, want %q", got.value, want)
			}
		})
	}
}

// ACL reads the same field upstream would: an unauthorized model in the last
// exact field is refused and never forwarded, whatever comes before it.
func TestMultipartUnauthorizedModelCannotRideAlong(t *testing.T) {
	for _, endpointType := range []string{config.EndpointTypeOpenAIImage, config.EndpointTypeOpenAI} {
		s, captured, principal := newMultipartService(t, endpointType)
		for _, parts := range [][]formPart{
			{{"model", "test-model", ""}, {"model", "gpt-secret", ""}},
			{{"Model", "test-model", ""}, {"model", "gpt-secret", ""}},
			{{"Model", "test-model", ""}},
		} {
			before := captured.count()
			w := serveMultipart(s, principal, "/v1/images/edits", buildMultipart(t, append([]formPart{{"image", "png", "a.png"}}, parts...)...))
			if w.Code < 400 || w.Code >= 500 || captured.count() != before {
				t.Fatalf("%s %v: status=%d forwarded=%v", endpointType, parts, w.Code, captured.count() != before)
			}
		}
	}
}
