package events

// Package events turns `pull` and `run` into something a supervisor can drive:
// stdout carries one JSON object per line and nothing else, and the model
// server's own output goes to `~/.fornax/server.log`. A heartbeat detects a
// supervisor that went away, so a served model never outlives its owner.

import (
	"encoding/json"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

const (
	ProgressEvery  = 250 * time.Millisecond
	heartbeatEvery = 5 * time.Second
)

type eventStream struct {
	mu     sync.Mutex
	enc    *json.Encoder
	gone   chan struct{}
	closed bool
}

var stream *eventStream

// Enable makes this process speak JSON lines on stdout.
func Enable() {
	// Without this a write to a closed pipe kills the process before the
	// model server is reaped; with it the write returns EPIPE instead.
	signal.Ignore(syscall.SIGPIPE)
	stream = &eventStream{enc: json.NewEncoder(os.Stdout), gone: make(chan struct{})}
}

func Emit(kind string, fields map[string]any) {
	if stream == nil {
		return
	}
	line := map[string]any{"event": kind}
	for key, value := range fields {
		line[key] = value
	}
	stream.mu.Lock()
	defer stream.mu.Unlock()
	if stream.closed {
		return
	}
	if err := stream.enc.Encode(line); err != nil {
		stream.closed = true
		close(stream.gone)
	}
}

// On reports whether this process is speaking events.
func On() bool { return stream != nil }

// SupervisorGone closes once nothing is reading stdout any more. Nil-safe:
// without `--events` it never fires.
func SupervisorGone() <-chan struct{} {
	if stream == nil {
		return nil
	}
	return stream.gone
}

func Heartbeat(done <-chan struct{}) {
	if stream == nil {
		return
	}
	tick := time.NewTicker(heartbeatEvery)
	defer tick.Stop()
	for {
		select {
		case <-done:
			return
		case <-stream.gone:
			return
		case <-tick.C:
			Emit("alive", nil)
		}
	}
}
