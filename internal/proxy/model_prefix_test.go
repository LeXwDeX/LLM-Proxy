package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

// Decode through the public config to exercise persistence and runtime publication.
func prefixConfig(t *testing.T, endpoint, epType, prefix string, mapped bool) *config.Config {
	t.Helper()
	target := map[string]any{"name": "prefix", "endpoint_type": epType, "endpoint": endpoint, "api_key": "fixture", "model_prefix": prefix}
	if mapped {
		target["model_mappings"] = []config.ModelMapping{{Upstream: "Fable5", Fallback: "fable"}}
	}
	raw, _ := json.Marshal(target)
	var tg config.Target
	if err := json.Unmarshal(raw, &tg); err != nil {
		t.Fatal(err)
	}
	return &config.Config{Server: config.ServerConfig{Bind: "127.0.0.1:0", RequestTimeoutSeconds: 5}, Targets: []config.Target{tg}}
}

func TestModelPrefixHTTPAndAccounting(t *testing.T) {
	for _, tc := range []struct {
		name, prefix, model, want string
		mapped                    bool
	}{
		{"mapped", "Vendor/", "fable", "Vendor/Fable5", true},
		{"empty", "", "fable", "Fable5", true},
		{"unmapped preserves case", "Vendor/", "FaBlE5", "Vendor/FaBlE5", false},
		{"exact concatenation", "Vendor", "fable", "VendorFable5", true},
		{"no dedup", "Fable", "fable", "FableFable5", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var received map[string]any
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				json.NewDecoder(r.Body).Decode(&received)
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"model":"Vendor/Fable5","choices":[{"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`)
			}))
			defer up.Close()
			s, err := NewService(prefixConfig(t, up.URL, "openai", tc.prefix, tc.mapped), newTestLogger())
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			recorder := &websocketUsageRecorder{}
			s.SetUsageRecorder(recorder)
			r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"`+tc.model+`","metadata":{"model":"fable"}}`))
			r.Header.Set("Content-Type", "application/json")
			r = r.WithContext(auth.WithPrincipal(r.Context(), newAllowAllPrincipal(t, "fixture")))
			w := httptest.NewRecorder()
			s.ServeHTTP(w, r)
			if w.Code != 200 {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
			if received["model"] != tc.want {
				t.Errorf("wire model=%v want=%s", received["model"], tc.want)
			}
			if received["metadata"].(map[string]any)["model"] != "fable" {
				t.Fatal("nested model modified")
			}
			events := recorder.snapshot()
			if len(events) != 1 || events[0].Model != "fable5" {
				t.Errorf("accounting identity=%+v", events)
			}
		})
	}
}

func TestModelPrefixPathAndMultipart(t *testing.T) {
	for _, tc := range []struct {
		name, epType, path string
		multipart          bool
	}{
		{"gemini", "gemini", "/v1beta/models/fable:generateContent?alt=sse", false},
		{"azure deployment", "azure_openai", "/openai/deployments/fable/chat/completions", false},
		{"multipart", "azure_openai", "/v1/images/edits", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotPath, gotQuery, gotModel string
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				gotQuery = r.URL.RawQuery
				if tc.multipart {
					r.ParseMultipartForm(1024)
					gotModel = r.FormValue("model")
				}
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"ok":true}`)
			}))
			defer up.Close()
			s, err := NewService(prefixConfig(t, up.URL, tc.epType, "Vendor/?x=#", true), newTestLogger())
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			var body bytes.Buffer
			ct := "application/json"
			if tc.multipart {
				m := multipart.NewWriter(&body)
				m.WriteField("model", "fable")
				m.WriteField("prompt", "fable")
				m.Close()
				ct = m.FormDataContentType()
			} else {
				body.WriteString(`{"model":"fable","contents":[]}`)
			}
			r := httptest.NewRequest("POST", tc.path, &body)
			r.Header.Set("Content-Type", ct)
			r = r.WithContext(auth.WithPrincipal(r.Context(), newAllowAllPrincipal(t, "fixture")))
			w := httptest.NewRecorder()
			s.ServeHTTP(w, r)
			if w.Code != 200 {
				t.Fatalf("status=%d %s", w.Code, w.Body.String())
			}
			if tc.multipart {
				if gotModel != "Vendor/?x=#Fable5" {
					t.Errorf("multipart model=%q", gotModel)
				}
			} else if !strings.Contains(gotPath, "Vendor/?x=#Fable5") {
				t.Errorf("path=%q", gotPath)
			}
			q, _ := url.ParseQuery(gotQuery)
			if q.Has("x") {
				t.Fatal("model injected query")
			}
		})
	}
}

func TestModelPrefixFailoverAndProbe(t *testing.T) {
	for _, epType := range []string{"openai", "gemini"} {
		t.Run(epType, func(t *testing.T) {
			var models, paths []string
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				model := extractModel(r, mustReadPrefixBody(t, r.Body))
				models = append(models, model)
				paths = append(paths, r.URL.Path)
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"ok":true}`)
			}))
			defer up.Close()
			cfg := prefixConfig(t, up.URL, epType, "Second/", true)
			primary := prefixConfig(t, "http://primary.local", epType, "First/", true).Targets[0]
			primary.Name = "primary"
			cfg.Targets = append([]config.Target{primary}, cfg.Targets...)
			s, err := NewService(cfg, newTestLogger())
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			u, _ := url.Parse(up.URL)
			s.httpClient = &http.Client{Transport: &failingTransport{successHost: u.Host}}
			path := "/v1/chat/completions"
			if epType == "gemini" {
				path = "/v1beta/models/fable:generateContent"
			}
			r := httptest.NewRequest("POST", path, strings.NewReader(`{"model":"fable"}`))
			r.Header.Set("Content-Type", "application/json")
			r = r.WithContext(auth.WithPrincipal(r.Context(), newAllowAllPrincipal(t, "fixture")))
			w := httptest.NewRecorder()
			s.ServeHTTP(w, r)
			if w.Code != 200 {
				t.Fatalf("failover: %d %s", w.Code, w.Body.String())
			}
			result, err := s.TestModel("prefix", "fable")
			if err != nil || !result.Success {
				t.Fatalf("probe: %+v %v", result, err)
			}
			// Gemini models live in the path rather than the body.
			if epType == "openai" {
				if len(models) != 2 || models[0] != "Second/Fable5" || models[1] != "Second/Fable5" {
					t.Fatalf("models=%v", models)
				}
			}
			if epType == "gemini" {
				if len(paths) != 2 || paths[0] != "/v1beta/models/Second/Fable5:generateContent" || paths[1] != paths[0] {
					t.Fatalf("failover/probe paths=%v", paths)
				}
			}
		})
	}
}

func mustReadPrefixBody(t *testing.T, r io.Reader) []byte {
	t.Helper()
	b, e := io.ReadAll(r)
	if e != nil {
		t.Error(e)
	}
	return b
}

func TestModelPrefixProbeUnlistedModel(t *testing.T) {
	var got string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = extractModel(r, mustReadPrefixBody(t, r.Body))
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true}`)
	}))
	defer up.Close()
	s, err := NewService(prefixConfig(t, up.URL, "openai", "Vendor/", true), newTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	result, err := s.TestModel("prefix", "OtherModel")
	if err != nil || !result.Success {
		t.Fatalf("probe=%+v err=%v", result, err)
	}
	if got != "Vendor/OtherModel" {
		t.Fatalf("probe model=%q", got)
	}
}
