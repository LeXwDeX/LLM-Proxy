package main

import (
	"bytes"
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func TestUsageClientIndexStartupDoesNotMigrateSynchronously(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, declaration := range file.Decls {
		fn, ok := declaration.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "main" {
			continue
		}
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if ok && selector.Sel.Name == "MigrateUsageClientIndex" {
				t.Error("startup directly waits for the full usage client index migration")
			}
			return true
		})
	}
}

// TestUsageProofIndexStartupOrder pins the Issue #114 startup sequence inside
// run(): the persistent odd-row proof index is built or resumed before the
// proof cache is warmed, and both finish before journal replay so replayed
// writes maintain an index that is already current.
func TestUsageProofIndexStartupOrder(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var runFn *ast.FuncDecl
	for _, declaration := range file.Decls {
		if fn, ok := declaration.(*ast.FuncDecl); ok && fn.Name.Name == "run" {
			runFn = fn
		}
	}
	if runFn == nil {
		t.Fatal("run() not found in main.go")
	}
	type startupCall struct {
		receiver string
		name     string
	}
	want := []startupCall{
		{receiver: "nosql", name: "MigrateUsageProofOddIndex"},
		{receiver: "nosql", name: "WarmUsageProofScanCache"},
		{receiver: "usageJournal", name: "Replay"},
	}
	positions := make(map[startupCall]token.Pos, len(want))
	ast.Inspect(runFn.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		receiver, ok := selector.X.(*ast.Ident)
		if !ok {
			return true
		}
		key := startupCall{receiver: receiver.Name, name: selector.Sel.Name}
		if _, seen := positions[key]; !seen {
			positions[key] = call.Pos()
		}
		return true
	})
	for _, call := range want {
		if _, ok := positions[call]; !ok {
			t.Fatalf("startup call %s.%s not found in run()", call.receiver, call.name)
		}
	}
	for i := 1; i < len(want); i++ {
		if positions[want[i-1]] >= positions[want[i]] {
			t.Errorf("startup order violated: %s.%s must precede %s.%s", want[i-1].receiver, want[i-1].name, want[i].receiver, want[i].name)
		}
	}
}

func TestUsageClientIndexMigrationRetriesWithoutRestart(t *testing.T) {
	var attempts atomic.Int64
	completed := make(chan struct{})
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	stop := startUsageClientIndexMigration(context.Background(), nil, logger, func(context.Context, *bolt.DB, int) error {
		if attempts.Add(1) == 1 {
			return errors.New("transient migration failure")
		}
		close(completed)
		return nil
	})
	defer stop()
	select {
	case <-completed:
	case <-time.After(2 * time.Second):
		t.Fatal("failed background migration was not retried")
	}
}

func TestUsageClientIndexMigrationNonBlockingAndJoinedOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	started, exited := make(chan struct{}), make(chan struct{})
	stopReturned := make(chan func(), 1)
	go func() {
		stopReturned <- startUsageClientIndexMigration(ctx, nil, logger, func(ctx context.Context, _ *bolt.DB, _ int) error {
			close(started)
			<-ctx.Done()
			close(exited)
			return ctx.Err()
		})
	}()
	var stop func()
	select {
	case stop = <-stopReturned:
	case <-time.After(2 * time.Second):
		t.Fatal("background migration blocked startup")
	}
	<-started
	cancel()
	stop()
	select {
	case <-exited:
	default:
		t.Fatal("stop did not join the migration worker")
	}
	stop() // repeated shutdown is safe
	if !strings.Contains(logs.String(), "interrupted; retry on next startup") {
		t.Fatalf("missing cancellation status: %s", logs.String())
	}
}

func TestUsageClientIndexMigrationFailureDoesNotGateStartup(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	failed := make(chan struct{})
	stop := startUsageClientIndexMigration(context.Background(), nil, logger, func(context.Context, *bolt.DB, int) error {
		close(failed)
		return errors.New("isolated migration failure")
	})
	<-failed
	stop()
	// Cancellation may race the failure log. Both are explicit, nonfatal status.
	if !strings.Contains(logs.String(), "legacy reads remain active; retry with backoff") && !strings.Contains(logs.String(), "interrupted; retry on next startup") {
		t.Fatalf("missing non-fatal failure status: %s", logs.String())
	}
}
