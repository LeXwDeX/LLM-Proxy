package proxy

import (
	"context"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	coderws "github.com/coder/websocket"
)

// The service's wire transport: TLS session resumption with HTTP/2 kept on,
// HTTP/2 dead-connection pings, a bounded dial timeout, and the existing pool
// and manual gzip negotiation fields unchanged.

func trustTestServer(t *testing.T, tr *http.Transport, server *httptest.Server) {
	t.Helper()
	if tr.TLSClientConfig == nil {
		t.Fatal("wire transport has no TLS client config")
	}
	pool := x509.NewCertPool()
	pool.AddCert(server.Certificate())
	tr.TLSClientConfig.RootCAs = pool
}

func TestWireTransportConfiguration(t *testing.T) {
	s, err := NewService(&config.Config{}, newTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	tr := s.automaticGzipTransport
	if s.httpClient.Transport != tr || s.responsesWS.automaticGzipTransport != tr {
		t.Fatal("transport identity changed; automatic gzip negotiation depends on it")
	}
	if !tr.DisableCompression || tr.MaxIdleConns != 100 || tr.MaxIdleConnsPerHost != 50 || tr.IdleConnTimeout != 90*time.Second || tr.TLSHandshakeTimeout != 10*time.Second || tr.Proxy == nil {
		t.Fatalf("existing transport fields changed: %+v", tr)
	}
	if tr.TLSClientConfig == nil || tr.TLSClientConfig.ClientSessionCache == nil {
		t.Fatal("TLS session cache missing")
	}
	if !tr.ForceAttemptHTTP2 {
		t.Fatal("a custom TLS config/dialer silently disables HTTP/2 without ForceAttemptHTTP2")
	}
	if tr.HTTP2 == nil || tr.HTTP2.SendPingTimeout != 30*time.Second || tr.HTTP2.PingTimeout != 15*time.Second {
		t.Fatalf("HTTP/2 ping config=%+v", tr.HTTP2)
	}
	if tr.DialContext == nil || upstreamDialTimeout != 8*time.Second {
		t.Fatalf("dial timeout=%v dialer=%v", upstreamDialTimeout, tr.DialContext != nil)
	}
}

func TestWireTransportTLSResumptionKeepsHTTP2(t *testing.T) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, r.Proto)
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	s, err := NewService(&config.Config{}, newTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	tr := s.automaticGzipTransport
	trustTestServer(t, tr, server)
	for i := 0; i < 2; i++ {
		req, _ := http.NewRequest(http.MethodGet, server.URL, nil)
		resp, err := s.doHTTP(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.ProtoMajor != 2 {
			t.Fatalf("request %d used %s; HTTP/2 must stay enabled", i, resp.Proto)
		}
		if i == 1 && (resp.TLS == nil || !resp.TLS.DidResume) {
			t.Fatal("second handshake did not resume the TLS session")
		}
		tr.CloseIdleConnections()
	}
}

// Upstream WebSocket dials share the wire transport and its session cache.
func TestWireTransportWebSocketDialResumes(t *testing.T) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := coderws.Accept(w, r, nil)
		if err != nil {
			return
		}
		c.Close(coderws.StatusNormalClosure, "")
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	s, err := NewService(&config.Config{}, newTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	tr := s.automaticGzipTransport
	trustTestServer(t, tr, server)
	url := "wss" + strings.TrimPrefix(server.URL, "https")
	for i := 0; i < 2; i++ {
		client := &http.Client{Transport: &responsesWSHandshakeTransport{base: tr, owned: tr}}
		conn, resp, err := coderws.Dial(context.Background(), url, &coderws.DialOptions{HTTPClient: client})
		if err != nil {
			t.Fatalf("dial %d: %v", i, err)
		}
		conn.CloseNow()
		if resp.TLS == nil {
			t.Fatal("no TLS state on the WebSocket handshake")
		}
		if i == 1 && !resp.TLS.DidResume {
			t.Fatal("second WebSocket dial did not resume the TLS session")
		}
	}
}

// realDialTimeout returns a genuine net package dial timeout error; it also
// matches context.DeadlineExceeded, which the classifier must not let hide
// the provable pre-send dial stage.
func realDialTimeout(t *testing.T) error {
	t.Helper()
	_, err := (&net.Dialer{Timeout: time.Nanosecond}).Dial("tcp", "127.0.0.1:9")
	var opErr *net.OpError
	if err == nil || !errors.As(err, &opErr) || opErr.Op != "dial" {
		t.Skipf("could not produce a dial timeout error: %v", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Logf("dial timeout error does not match DeadlineExceeded on this platform: %v", err)
	}
	return err
}

func dialTimeoutService(t *testing.T, upstreamURL string) *Service {
	t.Helper()
	s, err := NewService(&config.Config{Server: config.ServerConfig{RequestTimeoutSeconds: 10}, Targets: []config.Target{
		{Name: "primary", EndpointType: config.EndpointTypeOpenAI, Endpoint: "http://primary.invalid", APIKey: "k", UsageAdapter: config.UsageAdapterStandard, ModelMappings: []config.ModelMapping{{Upstream: "test-model"}}},
		{Name: "alternative", EndpointType: config.EndpointTypeOpenAI, Endpoint: upstreamURL, APIKey: "k", UsageAdapter: config.UsageAdapterStandard, ModelMappings: []config.ModelMapping{{Upstream: "test-model"}}},
	}}, newTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestDialTimeoutFailsOverBeforeSend(t *testing.T) {
	timeoutErr := realDialTimeout(t)
	var served atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served.Add(1)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer upstream.Close()
	s := dialTimeoutService(t, upstream.URL)
	tr := s.automaticGzipTransport
	base := tr.DialContext
	var primaryDials atomic.Int32
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if strings.HasPrefix(addr, "primary.invalid") {
			primaryDials.Add(1)
			return nil, timeoutErr
		}
		return base(ctx, network, addr)
	}
	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, goneRequest(t, "/v1/chat/completions", `{"model":"test-model","messages":[]}`))
	if rr.Code != 200 || served.Load() != 1 || primaryDials.Load() != 1 {
		t.Fatalf("dial timeout did not fail over: status=%d served=%d primary_dials=%d body=%s", rr.Code, served.Load(), primaryDials.Load(), rr.Body.String())
	}
	if s.MetricsSnapshot().TotalTargetRetries != 1 {
		t.Fatalf("target retries=%d want 1", s.MetricsSnapshot().TotalTargetRetries)
	}
}

// A dial interrupted by the request's own context (parent deadline or the
// per-attempt timeout) stays 504 and never fails over (#163).
func TestDialStoppedByContextDoesNotFailOver(t *testing.T) {
	for _, mode := range []string{"parent_deadline", "attempt_timeout"} {
		t.Run(mode, func(t *testing.T) {
			var served atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { served.Add(1) }))
			defer upstream.Close()
			s := dialTimeoutService(t, upstream.URL)
			tr := s.automaticGzipTransport
			base := tr.DialContext
			var primaryDials atomic.Int32
			tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
				if strings.HasPrefix(addr, "primary.invalid") {
					primaryDials.Add(1)
					<-ctx.Done()
					return nil, &net.OpError{Op: "dial", Net: network, Err: ctx.Err()}
				}
				return base(ctx, network, addr)
			}
			req := goneRequest(t, "/v1/chat/completions", `{"model":"test-model","messages":[]}`)
			if mode == "attempt_timeout" {
				s.setRequestTimeout(100 * time.Millisecond)
			} else {
				ctx, cancel := context.WithTimeout(req.Context(), 100*time.Millisecond)
				defer cancel()
				req = req.WithContext(ctx)
			}
			rr := httptest.NewRecorder()
			s.ServeHTTP(rr, req)
			if primaryDials.Load() != 1 {
				t.Fatalf("primary was not attempted first (dials=%d)", primaryDials.Load())
			}
			if rr.Code != http.StatusGatewayTimeout || served.Load() != 0 || s.MetricsSnapshot().TotalRetries != 0 {
				t.Fatalf("context-stopped dial failed over: status=%d served=%d retries=%d", rr.Code, served.Load(), s.MetricsSnapshot().TotalRetries)
			}
		})
	}
}
