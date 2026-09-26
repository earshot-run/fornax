package mcp

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"
)

func TestCancellationStopsAnIdleClient(t *testing.T) {
	input, client := io.Pipe()
	defer client.Close()
	defer input.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- run(ctx, "test", input, io.Discard) }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("MCP kept waiting for stdin after cancellation")
	}
}

type blockedInput struct{ release chan struct{} }

func (r *blockedInput) Read([]byte) (int, error) { <-r.release; return 0, io.EOF }
func (r *blockedInput) Close() error             { <-r.release; return nil }

func TestCancellationDoesNotWaitForStdinClose(t *testing.T) {
	input := &blockedInput{release: make(chan struct{})}
	defer close(input.release)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- run(ctx, "test", input, io.Discard) }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("MCP waited for a blocking stdin Close")
	}
}
