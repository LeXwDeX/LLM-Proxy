package proxy

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

func TestAuditRegressionConcurrentSelection(t *testing.T) {
	svc := newSingleTargetService(t)
	state := svc.targetOrder[0]
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			state.inFlight.Add(1)
			runtime.Gosched()
			state.inFlight.Add(-1)
		}
	}()
	defer func() { close(stop); wg.Wait() }()
	defer func() {
		if p := recover(); p != nil {
			t.Fatalf("selection panicked under concurrent traffic: %v", p)
		}
	}()
	for i := 0; i < 100000; i++ {
		selected, _, _ := svc.findAvailableTargetWithModel(nil, nil, nil, "", "/v1/chat/completions", time.Now())
		if selected == nil {
			t.Fatalf("unlimited healthy target disappeared on selection %d", i)
		}
	}
}

func TestAuditRegressionModelRewritePreservesPayload(t *testing.T) {
	body := []byte(`{"model":"alias","messages":[{"role":"user","content":"Explain alias please"}],"metadata":{"alias":"alias"}}`)
	var parsed map[string]any
	if err := json.Unmarshal(replaceModelInBody(body, "actual-model", "alias"), &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed["model"] != "actual-model" {
		t.Fatal("model was not rewritten")
	}
	message := parsed["messages"].([]any)[0].(map[string]any)["content"]
	if message != "Explain alias please" {
		t.Fatalf("rewrote user content: %v", message)
	}
}

type globalAuditRecorder struct{ events []usage.Event }

func (r *globalAuditRecorder) Record(e usage.Event) error { r.events = append(r.events, e); return nil }

func TestAuditRegressionCompressedUsage(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		gz := gzip.NewWriter(w)
		_, _ = gz.Write([]byte(`{"model":"test-model","usage":{"prompt_tokens":100,"completion_tokens":20}}`))
		_ = gz.Close()
	}))
	defer up.Close()
	svc, err := NewService(&config.Config{Server: config.ServerConfig{RequestTimeoutSeconds: 5}, Targets: []config.Target{{Name: "audit", EndpointType: config.EndpointTypeDualProtocol, Endpoint: up.URL, APIKey: "upstream-fixture"}}}, newTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	recorder := &globalAuditRecorder{}
	svc.SetUsageRecorder(recorder)
	p := newAllowAllPrincipal(t, "proxy-fixture")
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"test-model"}`))
	req.Header.Set("Accept-Encoding", "gzip")
	req = req.WithContext(auth.WithPrincipal(req.Context(), p))
	rr := httptest.NewRecorder()
	svc.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	if len(recorder.events) != 1 {
		t.Errorf("compressed success missing from usage: got %d events", len(recorder.events))
	}
}

func TestAuditRegressionHalfOpenCapacityLease(t *testing.T) {
	svc := newSingleTargetService(t)
	state := svc.targetOrder[0]
	state.Target().MaxConcurrency = 1
	now := time.Now()
	state.MarkFailure(now.Add(-2*time.Minute), time.Minute)
	state.inFlight.Store(1)
	svc.findAvailableTargetWithModel(nil, nil, nil, "", "/v1/chat/completions", now)
	state.inFlight.Store(0)
	selected, _, _ := svc.findAvailableTargetWithModel(nil, nil, nil, "", "/v1/chat/completions", now.Add(time.Second))
	if selected == nil {
		t.Fatal("capacity rejection orphaned the half-open lease")
	}
}

func TestAuditRegressionNativeGeminiNotRoutedToOpenAI(t *testing.T) {
	for _, typ := range []string{config.EndpointTypeOpenAI, config.EndpointTypeAzureOpenAI, config.EndpointTypeDualProtocol} {
		if PathSupportedByEndpointType(typ, "/v1beta/models/gemini-2.5-pro:generateContent") {
			t.Errorf("%s accepted native Gemini schema without a translator", typ)
		}
	}
}

func TestAuditRegressionMultipartModelPreservesAttachment(t *testing.T) {
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	_ = w.WriteField("model", "alias")
	_ = w.WriteField("prompt", "Explain alias please")
	file, _ := w.CreateFormFile("image", "alias.png")
	attachment := []byte("\x00\x01 alias \xff\xfe\n")
	_, _ = file.Write(attachment)
	_ = w.Close()
	rewritten := replaceModelInRequestBody(body.Bytes(), w.FormDataContentType(), "actual-model", "alias")
	r := multipart.NewReader(bytes.NewReader(rewritten), w.Boundary())
	for _, want := range []struct {
		name  string
		value []byte
	}{
		{"model", []byte("actual-model")}, {"prompt", []byte("Explain alias please")}, {"image", attachment},
	} {
		part, err := r.NextPart()
		if err != nil {
			t.Fatal(err)
		}
		value, err := io.ReadAll(part)
		if err != nil || part.FormName() != want.name || !bytes.Equal(value, want.value) {
			t.Fatalf("field %s changed: %q, %v", want.name, value, err)
		}
		if want.name == "image" && part.FileName() != "alias.png" {
			t.Fatal("filename changed")
		}
	}
}

func TestAuditRegressionUsageBeyondDiagnosticCapture(t *testing.T) {
	padding := strings.Repeat("x", 3*1024*1024)
	for _, test := range []struct{ name, contentType, body string }{
		{"large-json", "application/json", `{"choices":[{"message":{"content":"` + padding + `"}}],"usage":{"prompt_tokens":123,"completion_tokens":456}}`},
		{"middle-sse", "text/event-stream", strings.Repeat("data: {}\n\n", 20) + "data: {\"usage\":{\"prompt_tokens\":123,\"completion_tokens\":456}}\n\n" + strings.Repeat("data: {}\n\n", 20)},
		{"long-sse-event", "text/event-stream", "data: {\"choices\":[{\"delta\":{\"content\":\"" + padding + "\"}}],\"usage\":{\"prompt_tokens\":123,\"completion_tokens\":456}}\n\ndata: [DONE]\n\n"},
		{"nested-responses", "text/event-stream", "data: {\"response\":{\"output\":[1,{},null],\"usage\":{\"input_tokens\":123,\"output_tokens\":456}}}\n\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			observer := newUsageStream(test.contentType, "", usageParser{})
			capture := &limitedCaptureWriter{limit: 2 * 1024 * 1024}
			var forwarded bytes.Buffer
			_, err := io.Copy(&forwarded, io.TeeReader(strings.NewReader(test.body), io.MultiWriter(capture, observer)))
			observer.Close()
			if err != nil || forwarded.String() != test.body {
				t.Fatalf("response changed: %v", err)
			}
			if !observer.found || observer.tokens.InputTokens != 123 || observer.tokens.OutputTokens != 456 {
				t.Fatalf("usage was lost: %+v, found %v, error %v", observer.tokens, observer.found, observer.err)
			}
		})
	}
}

func TestAuditRegressionUsageParsingFailureStillForwards(t *testing.T) {
	for _, body := range []string{"not JSON", `{"usage":{"prompt_tokens":1}} trailing`, `{"choices":[`, ""} {
		observer := newUsageStream("application/json", "", usageParser{})
		var output bytes.Buffer
		_, err := io.Copy(&output, io.TeeReader(strings.NewReader(body), observer))
		observer.Close()
		if err != nil || output.String() != body || observer.found {
			t.Fatalf("malformed response mishandled: %v, %v", err, observer.found)
		}
	}
}

func TestAuditRegressionAffinityDoesNotReserveIncompatibleProbe(t *testing.T) {
	svc := newSingleTargetService(t)
	state := svc.targetOrder[0]
	p := newAllowAllPrincipal(t, "test")
	now := time.Now()
	state.MarkFailure(now.Add(-2*time.Minute), time.Minute)
	svc.affinity.Set(affinityKey("127.0.0.1", p.Name, ""), state.Target().Name, now)
	_, _, _, _ = svc.selectTarget(p, "127.0.0.1", "", "", nil, nil, "", "/v1/messages", now)
	selected, _, _, err := svc.selectTarget(p, "127.0.0.1", "", "", nil, nil, "", "/v1/chat/completions", now.Add(time.Second))
	if err != nil || selected != state {
		t.Fatalf("incompatible affinity occupied the probe: %v", err)
	}
}

func TestAuditRegressionReloadPreservesActualConcurrency(t *testing.T) {
	cfg := &config.Config{Targets: []config.Target{{Name: "one", EndpointType: config.EndpointTypeOpenAI, Endpoint: "http://127.0.0.1:1", APIKey: "k", MaxConcurrency: 1}}}
	svc, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	old := svc.targetOrder[0]
	if !old.acquireSlot() {
		t.Fatal("initial request rejected")
	}
	for i := 0; i < 3; i++ {
		if err := svc.ApplyConfig(cfg); err != nil {
			t.Fatal(err)
		}
		if svc.targetOrder[0].acquireSlot() {
			t.Fatal("reload allowed a second in-flight request at capacity one")
		}
	}
	old.releaseSlot()
	if !svc.targetOrder[0].acquireSlot() {
		t.Fatal("old response close did not free the shared slot")
	}
	svc.targetOrder[0].releaseSlot()
}
