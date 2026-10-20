package proxy

import (
	"context"
	"io"
	"net/http"
	"strings"
)

// automaticGzipNegotiation preserves net/http's default negotiation range while
// keeping response validation outside Transport, which reads only the first
// Content-Encoding field. An opaque custom RoundTripper owns its own policy.
func automaticGzipNegotiation(base http.RoundTripper, owned *http.Transport, req *http.Request) bool {
	if req.Method == http.MethodHead || req.Header.Get("Range") != "" || req.Header.Get("Accept-Encoding") != "" {
		return false
	}
	if base == nil {
		base = http.DefaultTransport
	}
	transport, ok := base.(*http.Transport)
	return ok && (transport == owned || !transport.DisableCompression)
}

// normalizeAutomaticGzip is lazy: a bad header/body is a post-send read failure,
// never a transport error that could replay an already-sent generation.
func normalizeAutomaticGzip(response *http.Response, ctx context.Context) {
	if response == nil || response.Body == nil || !strings.EqualFold(responseContentEncoding(response.Header), "gzip") {
		return
	}
	// net/http returns empty responses and bidirectional upgrade/tunnel bodies
	// before its automatic gzip branch. Preserve that compatibility boundary.
	if response.ContentLength == 0 {
		return
	}
	if _, writable := response.Body.(io.Writer); writable {
		return
	}
	decoded := &responsesWebsocketGzipBody{source: response.Body, ctx: ctx}
	decoded.stopCancel = context.AfterFunc(ctx, func() { _ = decoded.closeSource() })
	response.Body = decoded
	response.Header.Del("Content-Encoding")
	response.Header.Del("Content-Length")
	response.ContentLength = -1
	response.Uncompressed = true
}

func (s *Service) doHTTP(req *http.Request) (*http.Response, error) {
	client := s.httpClient
	if client == nil {
		client = http.DefaultClient
	}
	automatic := automaticGzipNegotiation(client.Transport, s.automaticGzipTransport, req)
	if automatic {
		req.Header.Set("Accept-Encoding", "gzip")
	}
	response, err := client.Do(req)
	if automatic && err == nil {
		normalizeAutomaticGzip(response, req.Context())
	}
	return response, err
}
