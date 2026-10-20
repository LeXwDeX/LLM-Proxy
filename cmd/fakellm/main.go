// Command fakellm runs the deterministic fake LLM provider used by tests,
// deployed mock acceptance and admin UI simulations. It never calls a real
// model. Environment variables keep the legacy Python mock provider names.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/fakellm"
)

// revision is injected with go build -ldflags '-X main.revision=<commit>'.
// Runtime environment cannot override the identity of the built artifact.
var revision = "dev"

type settings struct {
	Addr, Name, Model    string
	Delay, WSIdleTimeout time.Duration
	LegacyMock, Version  bool
}

func parseSettings(args []string, getenv func(string) string) (settings, error) {
	cfg := settings{Name: getenv("MOCK_NAME"), Model: getenv("MOCK_MODEL")}
	ints := map[string]int{"MOCK_PORT": 9000, "MOCK_DELAY_MS": 0, "MOCK_WS_IDLE_TIMEOUT_S": 15}
	for name, def := range ints {
		if raw := getenv(name); raw != "" {
			v, err := strconv.Atoi(raw)
			if err != nil {
				return cfg, fmt.Errorf("%s must be an integer", name)
			}
			ints[name] = v
		} else {
			ints[name] = def
		}
	}
	fs := flag.NewFlagSet("fakellm", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&cfg.Addr, "addr", "0.0.0.0:"+strconv.Itoa(ints["MOCK_PORT"]), "listen address (default 0.0.0.0:$MOCK_PORT or :9000)")
	fs.StringVar(&cfg.Name, "name", cfg.Name, "instance name (MOCK_NAME)")
	fs.StringVar(&cfg.Model, "model", cfg.Model, "model listed by /v1/models (MOCK_MODEL)")
	delay := fs.Int("delay-ms", ints["MOCK_DELAY_MS"], "default POST delay in milliseconds (MOCK_DELAY_MS)")
	idle := fs.Int("ws-idle-timeout-s", ints["MOCK_WS_IDLE_TIMEOUT_S"], "WebSocket idle timeout, clamped to 1–60 seconds")
	fs.BoolVar(&cfg.LegacyMock, "legacy-mock", false, "enable Python mock acceptance compatibility (fixed Responses usage 10/5)")
	fs.BoolVar(&cfg.Version, "version", false, "print build revision and exit")
	if err := fs.Parse(args); err != nil {
		return cfg, err
	}
	if fs.NArg() != 0 {
		return cfg, errors.New("unexpected positional arguments")
	}
	if *delay < 0 {
		return cfg, errors.New("delay-ms must not be negative")
	}
	if cfg.Addr == "" {
		return cfg, errors.New("addr must not be empty")
	}
	if cfg.Name == "" {
		cfg.Name = "fake"
		if cfg.LegacyMock {
			cfg.Name = "mock"
		}
	}
	if cfg.Model == "" {
		cfg.Model = "mock-model"
	}
	// Instance names become HTTP headers; fail instead of producing invalid ones.
	if strings.ContainsAny(cfg.Name, "\r\n") {
		return cfg, errors.New("name must not contain newlines")
	}
	cfg.Delay = time.Duration(*delay) * time.Millisecond
	cfg.WSIdleTimeout = time.Duration(min(60, max(1, *idle))) * time.Second
	return cfg, nil
}

func newHandler(cfg settings, buildRevision string) http.Handler {
	srv := fakellm.New(fakellm.Options{
		Name: cfg.Name, Model: cfg.Model, DefaultDelay: cfg.Delay,
		WSIdleTimeout: cfg.WSIdleTimeout, LegacyMock: cfg.LegacyMock,
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Mock-Instance", cfg.Name)
		if r.Method == http.MethodGet && r.URL.Path == "/healthz" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok", "instance": cfg.Name, "revision": buildRevision})
			return
		}
		if cfg.LegacyMock && strings.HasSuffix(r.URL.Path, ":streamGenerateContent") {
			// The Python mock always streams SSE, including without alt=sse.
			r = r.Clone(r.Context())
			q := r.URL.Query()
			q.Set("alt", "sse")
			r.URL.RawQuery = q.Encode()
		}
		if cfg.LegacyMock && r.Method == http.MethodGet && r.URL.Path == "/v1/responses" && !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			w.Header().Set("Upgrade", "websocket")
		}
		srv.ServeHTTP(w, r)
	})
}

func main() {
	cfg, err := parseSettings(os.Args[1:], os.Getenv)
	if errors.Is(err, flag.ErrHelp) {
		fmt.Println("fakellm [-addr host:port] [-name instance] [-model model] [-delay-ms N] [-ws-idle-timeout-s N] [-legacy-mock] [-version]\nEnvironment: MOCK_PORT, MOCK_NAME, MOCK_MODEL, MOCK_DELAY_MS, MOCK_WS_IDLE_TIMEOUT_S")
		return
	}
	if err != nil {
		log.Fatal(err)
	}
	if cfg.Version {
		fmt.Println(revision)
		return
	}
	listener, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		log.Fatal(err)
	}
	server := &http.Server{Handler: newHandler(cfg, revision), ReadHeaderTimeout: 10 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			_ = server.Close()
		}
	}()
	log.Printf("fakellm %q revision=%s legacy_mock=%t listening on %s", cfg.Name, revision, cfg.LegacyMock, listener.Addr())
	err = server.Serve(listener)
	stop()
	<-done
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
