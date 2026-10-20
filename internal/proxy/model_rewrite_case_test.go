package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

func TestReplaceModelInBodyRewritesCaseOnlyDifferences(t *testing.T) {
	for _, test := range []struct {
		name     string
		upstream string
		model    string
		body     string
		want     string
	}{
		{
			name:     "uppercase configured upstream",
			upstream: "GLM-5.3-FLASH",
			model:    "glm-5.3-flash",
			body:     `{"model":"glm-5.3-flash","messages":[{"role":"user","content":"GLM-5.3-FLASH"}]}`,
			want:     `{"model":"GLM-5.3-FLASH","messages":[{"role":"user","content":"GLM-5.3-FLASH"}]}`,
		},
		{
			name:     "lowercase configured upstream",
			upstream: "glm-5.3-flash",
			model:    "GLM-5.3-FLASH",
			body:     `{"model":"GLM-5.3-FLASH"}`,
			want:     `{"model":"glm-5.3-flash"}`,
		},
		{
			name:     "exact spelling needs no change",
			upstream: "glm-5.3-flash",
			model:    "glm-5.3-flash",
			body:     `{"model":"glm-5.3-flash"}`,
			want:     `{"model":"glm-5.3-flash"}`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := replaceModelInBody([]byte(test.body), test.upstream, test.model)
			var gotJSON, wantJSON any
			if err := json.Unmarshal(got, &gotJSON); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(test.want), &wantJSON); err != nil {
				t.Fatal(err)
			}
			if !jsonEqual(gotJSON, wantJSON) {
				t.Fatalf("rewritten JSON = %s, want %s", got, test.want)
			}
		})
	}
}

func TestReplaceModelInRequestBodyCaseOnlyPreservesMultipartAttachment(t *testing.T) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("model", "glm-5.3-flash"); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteField("prompt", "GLM-5.3-FLASH stays in the prompt"); err != nil {
		t.Fatal(err)
	}
	attachment := []byte("\x00GLM-5.3-FLASH\xff\n")
	file, err := writer.CreateFormFile("image", "model.bin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(attachment); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	rewritten := replaceModelInRequestBody(body.Bytes(), writer.FormDataContentType(), "GLM-5.3-FLASH", "glm-5.3-flash")
	reader := multipart.NewReader(bytes.NewReader(rewritten), writer.Boundary())
	want := map[string][]byte{
		"model":  []byte("GLM-5.3-FLASH"),
		"prompt": []byte("GLM-5.3-FLASH stays in the prompt"),
		"image":  attachment,
	}
	for range len(want) {
		part, err := reader.NextPart()
		if err != nil {
			t.Fatal(err)
		}
		value, err := io.ReadAll(part)
		if err != nil {
			t.Fatal(err)
		}
		if expected, ok := want[part.FormName()]; !ok || !bytes.Equal(value, expected) {
			t.Fatalf("multipart field %q = %q, want %q", part.FormName(), value, expected)
		}
		delete(want, part.FormName())
		if part.FormName() == "image" && part.FileName() != "model.bin" {
			t.Fatalf("attachment filename changed to %q", part.FileName())
		}
	}
	if len(want) != 0 {
		t.Fatalf("multipart fields missing: %v", want)
	}
}

func TestReplaceModelInURLPathCaseOnlyDifferences(t *testing.T) {
	for _, test := range []struct {
		name     string
		path     string
		upstream string
		model    string
		want     string
	}{
		{
			name:     "Azure deployment path",
			path:     "/openai/deployments/glm-5.3-flash/chat/completions?api-version=2025-01-01",
			upstream: "GLM-5.3-FLASH",
			model:    "glm-5.3-flash",
			want:     "/openai/deployments/GLM-5.3-FLASH/chat/completions?api-version=2025-01-01",
		},
		{
			name:     "Gemini model path",
			path:     "/v1beta/models/GLM-5.3-FLASH:generateContent?key=proxy-key",
			upstream: "glm-5.3-flash",
			model:    "GLM-5.3-FLASH",
			want:     "/v1beta/models/glm-5.3-flash:generateContent?key=proxy-key",
		},
		{
			name:     "same exact path model",
			path:     "/v1beta/models/glm-5.3-flash:generateContent",
			upstream: "glm-5.3-flash",
			model:    "glm-5.3-flash",
			want:     "/v1beta/models/glm-5.3-flash:generateContent",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := replaceModelInURLPath(test.path, test.upstream, test.model); got != test.want {
				t.Fatalf("rewritten path = %q, want %q", got, test.want)
			}
		})
	}
}

func TestExtractModelPreservesURLPathCase(t *testing.T) {
	for _, test := range []struct {
		path string
		want string
	}{
		{"/v1beta/models/GlM-5.3-FlAsH:generateContent", "GlM-5.3-FlAsH"},
		{"/api/İ/models/GlM-5.3-FlAsH:generateContent", "GlM-5.3-FlAsH"},
		{"/openai/deployments/GlM-5.3-FlAsH/chat/completions?api-version=2025-01-01", "GlM-5.3-FlAsH"},
		{"/openai/Deployments/GLM-5.3-FLASH/chat/completions", "GLM-5.3-FLASH"},
	} {
		request := httptest.NewRequest(http.MethodPost, test.path, nil)
		if got := extractModel(request, nil); got != test.want {
			t.Errorf("path %q: extracted model = %q, want original spelling %q", test.path, got, test.want)
		}
	}
}

func TestServiceRewritesAzureDeploymentURLToConfiguredSpelling(t *testing.T) {
	var receivedPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	mapping := config.ModelMapping{Upstream: "GLM-5.3-FLASH"}
	service, err := NewService(&config.Config{
		Server:  config.ServerConfig{Bind: "127.0.0.1:0", RequestTimeoutSeconds: 5},
		Targets: []config.Target{{Name: "azure-glm", EndpointType: config.EndpointTypeAzureOpenAI, Endpoint: upstream.URL, APIKey: "fixture", ModelMappings: []config.ModelMapping{mapping}}},
	}, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	principal := newAllowAllPrincipal(t, "fixture-azure-token")
	request := httptest.NewRequest(http.MethodPost, "/openai/deployments/glm-5.3-flash/chat/completions?api-version=2025-01-01", bytes.NewBufferString(`{"model":"glm-5.3-flash"}`))
	request.Header.Set("Content-Type", "application/json")
	request = request.WithContext(auth.WithPrincipal(request.Context(), principal))
	response := httptest.NewRecorder()
	service.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("proxy status = %d, body = %s", response.Code, response.Body.String())
	}
	if receivedPath != "/openai/deployments/GLM-5.3-FLASH/chat/completions" {
		t.Fatalf("upstream path = %q, want the configured upstream spelling in the deployment segment", receivedPath)
	}
}

func TestServiceForwardsExactConfiguredModelSpelling(t *testing.T) {
	for _, test := range []struct {
		name     string
		request  string
		upstream string
		fallback string
	}{
		{name: "client alias", request: "glm-flash", upstream: "GLM-5.3-FLASH", fallback: "glm-flash"},
		{name: "lowercase request to uppercase provider id", request: "glm-5.3-flash", upstream: "GLM-5.3-FLASH"},
		{name: "mixed-case request to lowercase provider id", request: "GlM-5.3-FlAsH", upstream: "glm-5.3-flash"},
		{name: "exact model id", request: "glm-5.3-flash", upstream: "glm-5.3-flash"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var received []byte
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var err error
				received, err = io.ReadAll(r.Body)
				if err != nil {
					t.Errorf("read upstream request body: %v", err)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"ok":true}`))
			}))
			defer upstream.Close()

			mapping := config.ModelMapping{Upstream: test.upstream, Fallback: test.fallback}
			service, err := NewService(&config.Config{
				Server:  config.ServerConfig{Bind: "127.0.0.1:0", RequestTimeoutSeconds: 5},
				Targets: []config.Target{{Name: "glm", EndpointType: config.EndpointTypeOpenAI, Endpoint: upstream.URL, APIKey: "fixture", ModelMappings: []config.ModelMapping{mapping}}},
			}, newTestLogger())
			if err != nil {
				t.Fatalf("NewService: %v", err)
			}
			principal := newAllowAllPrincipal(t, "fixture-token")
			body := `{"model":"` + test.request + `","messages":[{"role":"user","content":"GLM-5.3-FLASH"}],"metadata":{"model":"GLM-5.3-FLASH"},"tools":[{"function":{"arguments":"{\"model\":\"GLM-5.3-FLASH\"}"}}]}`
			request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
			request.Header.Set("Content-Type", "application/json")
			request = request.WithContext(auth.WithPrincipal(request.Context(), principal))
			response := httptest.NewRecorder()
			service.ServeHTTP(response, request)
			if response.Code != http.StatusOK {
				t.Fatalf("proxy status = %d, body = %s", response.Code, response.Body.String())
			}

			var actual map[string]any
			if err := json.Unmarshal(received, &actual); err != nil {
				t.Fatalf("upstream body is invalid JSON: %v", err)
			}
			if got := actual["model"]; got != test.upstream {
				t.Fatalf("upstream received model %v, want exact configured value %q", got, test.upstream)
			}
			messages := actual["messages"].([]any)
			if got := messages[0].(map[string]any)["content"]; got != "GLM-5.3-FLASH" {
				t.Fatalf("user content changed: %v", got)
			}
			if got := actual["metadata"].(map[string]any)["model"]; got != "GLM-5.3-FLASH" {
				t.Fatalf("metadata changed: %v", got)
			}
			toolArgs := actual["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)["arguments"]
			if toolArgs != `{"model":"GLM-5.3-FLASH"}` {
				t.Fatalf("tool arguments changed: %v", toolArgs)
			}
		})
	}
}

func TestServiceFailoverUsesEachTargetsOwnModelSpelling(t *testing.T) {
	var receivedModel string
	secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var requestBody map[string]any
		if err := json.NewDecoder(r.Body).Decode(&requestBody); err != nil {
			t.Errorf("decode upstream request: %v", err)
		} else {
			receivedModel, _ = requestBody["model"].(string)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer secondary.Close()
	secondaryURL, err := url.Parse(secondary.URL)
	if err != nil {
		t.Fatal(err)
	}

	service, err := NewService(&config.Config{
		Server: config.ServerConfig{Bind: "127.0.0.1:0", RequestTimeoutSeconds: 5},
		Targets: []config.Target{
			{Name: "primary", EndpointType: config.EndpointTypeOpenAI, Endpoint: "http://primary.local", APIKey: "primary-key", ModelMappings: []config.ModelMapping{{Upstream: "GLM-5.3-FLASH", Fallback: "glm-flash"}}},
			{Name: "secondary", EndpointType: config.EndpointTypeOpenAI, Endpoint: secondary.URL, APIKey: "secondary-key", ModelMappings: []config.ModelMapping{{Upstream: "glm-5.3-flash", Fallback: "glm-flash"}}},
		},
	}, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	service.httpClient = &http.Client{Transport: &failingTransport{successHost: secondaryURL.Host}}
	service.quietPeriod = 10 * time.Millisecond
	principal := newAllowAllPrincipal(t, "fixture-failover-token")
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"glm-flash"}`))
	request.Header.Set("Content-Type", "application/json")
	request = request.WithContext(auth.WithPrincipal(request.Context(), principal))
	response := httptest.NewRecorder()
	service.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("proxy status = %d, body = %s", response.Code, response.Body.String())
	}
	if got := response.Header().Get("X-Proxy-Target"); got != "secondary" {
		t.Fatalf("selected target = %q, want secondary", got)
	}
	if receivedModel != "glm-5.3-flash" {
		t.Fatalf("failover upstream received model %q, want its configured spelling %q", receivedModel, "glm-5.3-flash")
	}
}

func TestServicePreservesOriginalModelSpellingWithoutMappings(t *testing.T) {
	const requestedModel = "GLM-5.3-FLASH"
	var receivedModel string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var requestBody map[string]any
		if err := json.NewDecoder(r.Body).Decode(&requestBody); err != nil {
			t.Errorf("decode upstream request: %v", err)
		} else {
			receivedModel, _ = requestBody["model"].(string)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	service, err := NewService(&config.Config{
		Server:  config.ServerConfig{Bind: "127.0.0.1:0", RequestTimeoutSeconds: 5},
		Targets: []config.Target{{Name: "unmapped", EndpointType: config.EndpointTypeOpenAI, Endpoint: upstream.URL, APIKey: "fixture"}},
	}, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	principal := newAllowAllPrincipal(t, "fixture-unmapped-token")
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"`+requestedModel+`"}`))
	request.Header.Set("Content-Type", "application/json")
	request = request.WithContext(auth.WithPrincipal(request.Context(), principal))
	response := httptest.NewRecorder()
	service.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("proxy status = %d, body = %s", response.Code, response.Body.String())
	}
	if receivedModel != requestedModel {
		t.Fatalf("unmapped upstream received model %q, want original spelling %q", receivedModel, requestedModel)
	}
}

func jsonEqual(left, right any) bool {
	leftJSON, err := json.Marshal(left)
	if err != nil {
		return false
	}
	rightJSON, err := json.Marshal(right)
	return err == nil && bytes.Equal(leftJSON, rightJSON)
}
