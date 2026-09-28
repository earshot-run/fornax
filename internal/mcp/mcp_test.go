package mcp

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
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

func TestOutPathUsesFornaxOutDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "made")
	t.Setenv("FORNAX_OUT_DIR", dir)
	got, err := outPath("", "imagine", "png")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(got) != dir || !strings.HasPrefix(filepath.Base(got), "imagine-") {
		t.Fatalf("default name landed at %s", got)
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		t.Fatalf("out dir not created: %v", err)
	}
	if got, _ := outPath("clips/a.webm", "animate", "webm"); got != filepath.Join(dir, "clips/a.webm") {
		t.Fatalf("relative out landed at %s", got)
	}
	abs := filepath.Join(t.TempDir(), "b.wav")
	if got, _ := outPath(abs, "say", "wav"); got != abs {
		t.Fatalf("absolute out moved to %s", got)
	}
	t.Setenv("FORNAX_OUT_DIR", "")
	if got, _ := outPath("c.png", "imagine", "png"); got != "c.png" {
		t.Fatalf("without FORNAX_OUT_DIR, out moved to %s", got)
	}
}
