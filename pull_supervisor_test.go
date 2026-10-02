package main

import (
	"context"
	"io"
	"testing"
	"time"
)

func TestSupervisedPullStopsWithItsReader(t *testing.T) {
	t.Setenv("FORNAX_HOME", t.TempDir())
	gone := make(chan struct{})
	ctx, cancel := supervisedPullContext(context.Background(), gone)
	defer cancel()
	select {
	case <-ctx.Done():
		t.Fatal("pull canceled while reader present")
	default:
	}
	close(gone)
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("pull outlived its reader")
	}
}

func TestSupervisedPullStopsWithParent(t *testing.T) {
	t.Setenv("FORNAX_HOME", t.TempDir())
	parent, stop := context.WithCancel(context.Background())
	ctx, cancel := supervisedPullContext(parent, make(chan struct{}))
	defer cancel()
	stop()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("pull ignored cancellation")
	}
}

func TestPullInputLeaseStopsImmediatelyAtEOF(t *testing.T) {
	t.Setenv("FORNAX_HOME", t.TempDir())
	reader, writer := io.Pipe()
	defer reader.Close()
	ctx, cancel := inputLeashedPull(context.Background(), reader)
	defer cancel()
	select {
	case <-ctx.Done():
		t.Fatal("pull canceled while lease open")
	default:
	}
	writer.Close()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("pull outlived its studio lease")
	}
}
