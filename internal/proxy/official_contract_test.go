package proxy

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

// These are curated contract samples, not a substitute for the entire vendor
// schema. Each family owns its fixture and sources; this runner knows no usage
// formulas. Expected counters are independent literal values in the fixtures.
type officialContract struct {
	Name             string            `json:"name"`
	Source           string            `json:"source"`
	EndpointType     string            `json:"endpoint_type"`
	Adapter          string            `json:"adapter"`
	Model            string            `json:"model"`
	BasePath         string            `json:"base_path"`
	ClientPath       string            `json:"client_path"`
	UpstreamPath     string            `json:"upstream_path"`
	Request          json.RawMessage   `json:"request"`
	Headers          map[string]string `json:"headers"`
	AuthHeader       string            `json:"auth_header"`
	AuthValue        string            `json:"auth_value"`
	Response         string            `json:"response"`
	ContentType      string            `json:"content_type"`
	Status           int               `json:"status"`
	ResponseEncoding string            `json:"response_encoding"`
	RequestIDHeader  string            `json:"request_id_header"`
	Want             map[string]any    `json:"want"`
}

func runOfficialContracts(t *testing.T, family string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "official", family+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var cases []officialContract
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err = d.Decode(&cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) == 0 {
		t.Fatal("empty contract suite")
	}
	names := map[string]bool{}
	for _, tc := range cases {
		if names[tc.Name] || tc.Source == "" {
			t.Fatalf("missing source or duplicate case %q", tc.Name)
		}
		names[tc.Name] = true
		t.Run(tc.Name, func(t *testing.T) { runOfficialContract(t, tc) })
	}
}

func runOfficialContract(t *testing.T, tc officialContract) {
	t.Helper()
	var expected map[string]any
	if err := json.Unmarshal(tc.Request, &expected); err != nil {
		t.Fatal(err)
	}
	if err := validateContractRequest(tc.ClientPath, expected); err != nil {
		t.Fatal(err)
	}
	wire := []byte(tc.Response)
	if tc.ResponseEncoding == "gzip" {
		var compressed bytes.Buffer
		zw := gzip.NewWriter(&compressed)
		if _, err := zw.Write(wire); err != nil {
			t.Fatal(err)
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		wire = compressed.Bytes()
	}
	var hits atomic.Int32
	checks := make(chan error, 2)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		var actual map[string]any
		err := json.NewDecoder(r.Body).Decode(&actual)
		if err == nil && (r.Method != "POST" || r.URL.RequestURI() != tc.UpstreamPath) {
			err = fmt.Errorf("request route %s %s, want POST %s", r.Method, r.URL.RequestURI(), tc.UpstreamPath)
		}
		if err == nil && !reflect.DeepEqual(actual, expected) {
			err = fmt.Errorf("outbound body changed: got %v want %v", actual, expected)
		}
		if err == nil && r.Header.Get(tc.AuthHeader) != tc.AuthValue {
			err = fmt.Errorf("wrong upstream auth header %s", tc.AuthHeader)
		}
		for _, h := range []string{"Authorization", "X-Api-Key", "X-Goog-Api-Key", "Api-Key"} {
			if strings.Contains(r.Header.Get(h), "client-secret") {
				err = fmt.Errorf("client credential leaked in %s", h)
			}
		}
		for k, v := range tc.Headers {
			if r.Header.Get(k) != v {
				err = fmt.Errorf("header %s changed", k)
			}
		}
		if err != nil {
			checks <- err
			http.Error(w, "invalid official request", 400)
			return
		}
		checks <- nil
		w.Header().Set("Content-Type", tc.ContentType)
		if tc.ResponseEncoding != "" {
			w.Header().Set("Content-Encoding", tc.ResponseEncoding)
		}
		idHeader := tc.RequestIDHeader
		if idHeader == "" {
			idHeader = "X-Request-Id"
		}
		w.Header().Set(idHeader, "official-fixture-request")
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(tc.Status)
		// Split UTF-8 and JSON/SSE boundaries deliberately; transport fragments must
		// not become parser events. Every byte must still reach the downstream.
		for offset := 0; offset < len(wire); {
			end := min(offset+7, len(wire))
			_, _ = w.Write(wire[offset:end])
			w.(http.Flusher).Flush()
			offset = end
		}
	}))
	defer upstream.Close()
	cfg := &config.Config{Server: config.ServerConfig{RequestTimeoutSeconds: 5}, Targets: []config.Target{{Name: "official", Endpoint: upstream.URL + tc.BasePath, EndpointType: tc.EndpointType, UsageAdapter: tc.Adapter, APIKey: "upstream-secret", SupportsResponses: true, ModelMappings: []config.ModelMapping{{Upstream: tc.Model, Fallback: "public-alias"}}}}}
	svc, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	recorder := &adapterEventRecorder{}
	svc.SetUsageRecorder(recorder)
	store := auth.NewStore()
	if err = store.LoadFromConfig(testAuthClients("contract-client", "client-secret")); err != nil {
		t.Fatal(err)
	}
	principal, _ := store.Authenticate("client-secret")
	clientBody := map[string]any{}
	for k, v := range expected {
		clientBody[k] = v
	}
	if _, ok := clientBody["model"]; ok {
		clientBody["model"] = "public-alias"
	}
	body, _ := json.Marshal(clientBody)
	path := strings.Replace(tc.ClientPath, tc.Model, "public-alias", 1)
	req := httptest.NewRequest("POST", path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if tc.ResponseEncoding != "" {
		req.Header.Set("Accept-Encoding", tc.ResponseEncoding)
	}
	req.Header.Set("Authorization", "Bearer client-secret")
	req.Header.Set("X-Api-Key", "client-secret")
	req.Header.Set("X-Goog-Api-Key", "client-secret")
	for k, v := range tc.Headers {
		req.Header.Set(k, v)
	}
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))
	rr := httptest.NewRecorder()
	svc.ServeHTTP(rr, req)
	if hits.Load() != 1 {
		t.Fatalf("upstream calls=%d, status=%d body=%s", hits.Load(), rr.Code, rr.Body.String())
	}
	if err := <-checks; err != nil {
		t.Fatal(err)
	}
	if rr.Code != tc.Status || !bytes.Equal(rr.Body.Bytes(), wire) {
		t.Fatalf("response altered: status=%d want=%d body=%q", rr.Code, tc.Status, rr.Body.String())
	}
	if tc.ResponseEncoding != "" && rr.Header().Get("Content-Encoding") != tc.ResponseEncoding {
		t.Error("response encoding changed")
	}
	if rr.Header().Get("X-Upstream-Request-Id") != "official-fixture-request" {
		t.Error("request ID lost")
	}
	if tc.Status >= 400 {
		if rr.Header().Get("Retry-After") != "1" {
			t.Error("Retry-After lost")
		}
		if len(recorder.events) != 0 {
			t.Fatalf("HTTP error billed: %+v", recorder.events)
		}
		return
	}
	if len(recorder.events) != 1 {
		t.Fatalf("usage records=%d", len(recorder.events))
	}
	e := recorder.events[0]
	raw, _ := json.Marshal(e)
	var actual map[string]any
	_ = json.Unmarshal(raw, &actual)
	if e.Model != tc.Model {
		t.Errorf("billing model=%q want %q", e.Model, tc.Model)
	}
	for k, want := range tc.Want {
		got, ok := actual[k]
		if !ok {
			switch want.(type) {
			case float64:
				got = float64(0)
			case bool:
				got = false
			case string:
				got = ""
			}
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s=%v want %v; event=%s", k, got, want, raw)
		}
	}
}

// Fixture lint catches accidental mixing of incompatible request envelopes.
// It intentionally permits extensions: the production proxy must be transparent.
func validateContractRequest(path string, body map[string]any) error {
	var required string
	var forbidden []string
	switch {
	case strings.Contains(path, ":generateContent"), strings.Contains(path, ":streamGenerateContent"):
		required = "contents"
		forbidden = []string{"messages", "input", "model", "stream"}
	case strings.HasSuffix(path, "/responses"):
		required = "input"
		forbidden = []string{"messages", "contents", "max_tokens"}
	case strings.HasSuffix(path, "/messages"):
		required = "messages"
		forbidden = []string{"input", "contents", "max_completion_tokens"}
		if _, ok := body["max_tokens"]; !ok {
			return fmt.Errorf("Messages requires max_tokens")
		}
	default:
		required = "messages"
		forbidden = []string{"input", "contents"}
	}
	if _, ok := body[required]; !ok {
		return fmt.Errorf("missing %s", required)
	}
	for _, k := range forbidden {
		if _, ok := body[k]; ok {
			return fmt.Errorf("cross-protocol field %s", k)
		}
	}
	return nil
}

func TestOfficialContractFixtureRejectsMixedEnvelopes(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages", "/v1beta/models/model:generateContent"} {
		if validateContractRequest(path, map[string]any{"messages": []any{}, "input": "hello", "contents": []any{}, "max_tokens": 64}) == nil {
			t.Errorf("mixed fixture accepted: %s", path)
		}
	}
}
