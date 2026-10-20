package fakellm

import (
	"compress/gzip"
	"compress/zlib"
	"fmt"
	"io"
	"net/http"

	"github.com/klauspost/compress/zstd"
)

func validateResponseEncoding(sc *Scenario) error {
	if sc == nil {
		return nil
	}
	switch sc.ResponseEncoding {
	case "", "identity", "gzip", "deflate", "zstd":
	default:
		return fmt.Errorf("unsupported response_encoding %q", sc.ResponseEncoding)
	}
	switch sc.ResponseEncodingFault {
	case "":
	case "truncated", "corrupt":
		if sc.ResponseEncoding == "" || sc.ResponseEncoding == "identity" {
			return fmt.Errorf("response_encoding_fault requires gzip, deflate or zstd")
		}
	default:
		return fmt.Errorf("unsupported response_encoding_fault %q", sc.ResponseEncodingFault)
	}
	for _, header := range sc.ResponseEncodingHeaders {
		for _, r := range header {
			if r == '\r' || r == '\n' || r == 127 || r < 32 && r != '\t' {
				return fmt.Errorf("invalid response_encoding_headers value")
			}
		}
	}
	return nil
}

type responseEncoder interface {
	io.WriteCloser
	Flush() error
}

// The sink can be detached before Close to release encoder resources without
// repairing an intentionally aborted/truncated response on the client wire.
type encodingSink struct{ writer io.Writer }

func (s *encodingSink) Write(p []byte) (int, error) { return s.writer.Write(p) }

type encodedResponseWriter struct {
	http.ResponseWriter
	encoder                        responseEncoder
	sink                           *encodingSink
	encoding, fault                string
	headers                        []string
	wroteHeader, started, finished bool
	err                            error
}

func newEncodedResponseWriter(w http.ResponseWriter, sc *Scenario) (*encodedResponseWriter, error) {
	if err := validateResponseEncoding(sc); err != nil {
		return nil, err
	}
	cw := &encodedResponseWriter{ResponseWriter: w, encoding: sc.ResponseEncoding, fault: sc.ResponseEncodingFault, headers: sc.ResponseEncodingHeaders, sink: &encodingSink{writer: w}}
	switch sc.ResponseEncoding {
	case "gzip":
		cw.encoder = gzip.NewWriter(cw.sink)
	case "deflate":
		cw.encoder = zlib.NewWriter(cw.sink)
	case "zstd":
		var err error
		cw.encoder, err = zstd.NewWriter(cw.sink, zstd.WithEncoderConcurrency(1))
		if err != nil {
			return nil, err
		}
	}
	return cw, nil
}

func (w *encodedResponseWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	h := w.Header()
	h.Del("Content-Encoding")
	if w.headers != nil {
		for _, value := range w.headers {
			h.Add("Content-Encoding", value)
		}
	} else if w.encoding != "" {
		h.Set("Content-Encoding", w.encoding)
	}
	// A length set by the producer describes plaintext, never encoded bytes.
	if w.encoder != nil {
		h.Del("Content-Length")
	}
	w.ResponseWriter.WriteHeader(status)
}
func (w *encodedResponseWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if w.err != nil {
		return 0, w.err
	}
	w.startEncoding()
	if w.err != nil {
		return 0, w.err
	}
	var n int
	if w.encoder == nil {
		n, w.err = w.ResponseWriter.Write(p)
	} else {
		n, w.err = w.encoder.Write(p)
	}
	return n, w.err
}
func (w *encodedResponseWriter) Flush() {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	w.startEncoding()
	if w.err == nil && w.encoder != nil {
		w.err = w.encoder.Flush()
	}
	if w.err == nil {
		if f, ok := w.ResponseWriter.(http.Flusher); ok {
			f.Flush()
		}
	}
}

// finish runs only after a normal handler return. Application stream errors
// are valid encoded responses; cancellation and write failure are not.
func (w *encodedResponseWriter) finish(r *http.Request) {
	if w.finished {
		return
	}
	if r.Context().Err() != nil || w.err != nil {
		w.release()
		return
	}
	if w.encoder != nil {
		if w.fault == "truncated" {
			w.err = w.encoder.Flush()
			w.release()
			return
		}
		w.err = w.encoder.Close()
	}
	w.finished = true
}
func (w *encodedResponseWriter) release() {
	if w.finished {
		return
	}
	w.finished = true
	if w.encoder != nil {
		w.sink.writer = io.Discard
		_ = w.encoder.Close()
	}
}

// Flush can precede the first application Write (SSE headers). A format fault
// must precede even the encoder header emitted by that initial flush.
func (w *encodedResponseWriter) startEncoding() {
	if !w.started {
		w.started = true
		if w.fault == "corrupt" {
			_, w.err = w.sink.Write([]byte("invalid-compression-prefix"))
		}
	}
}
