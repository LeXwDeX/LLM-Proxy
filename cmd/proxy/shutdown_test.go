package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"
)

// The serve error path drains like the normal path: hijacked WebSocket turns
// get the shutdown grace to finish and record usage before persistence is
// drained and the database closes.
func TestServeAndDrainErrorPathDrainsWebsockets(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server := &http.Server{Addr: busy.Addr().String(), Handler: http.NotFoundHandler()}
	drained := make(chan time.Duration, 1)
	err = serveAndDrain(context.Background(), server, 5*time.Second, logger, func(ctx context.Context) error {
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Error("WebSocket drain on the error path must be bounded by the shutdown grace")
		}
		drained <- time.Until(deadline)
		return nil
	})
	if err == nil {
		t.Fatal("listen on a busy address must fail")
	}
	select {
	case left := <-drained:
		if left <= 0 || left > 5*time.Second {
			t.Errorf("drain deadline %v outside the grace", left)
		}
	default:
		t.Fatal("serve error path skipped the WebSocket drain")
	}
}

// finishShutdown is shared by the normal and the error path: stop background
// work, close the proxy service (cancels and waits for WebSocket turns, which
// record usage), then drain persistence. A service close error does not skip
// the persistence drain.
func TestFinishShutdownOrder(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, closeErr := range []error{nil, errors.New("close failed")} {
		var order []string
		finishShutdown(logger,
			func() { order = append(order, "stop") },
			func() error { order = append(order, "service"); return closeErr },
			func() { order = append(order, "persistence") },
		)
		if len(order) != 3 || order[0] != "stop" || order[1] != "service" || order[2] != "persistence" {
			t.Fatalf("closeErr=%v order=%v", closeErr, order)
		}
	}
}
