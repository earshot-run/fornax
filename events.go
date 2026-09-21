package main

// `--events` turns `pull` and `run` into something a supervisor can drive:
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
	progressEvery  = 250 * time.Millisecond
	heartbeatEvery = 5 * time.Second
)

type eventStream struct {
	mu     sync.Mutex
	enc    *json.Encoder
	gone   chan struct{}
	closed bool
}

var events *eventStream

func enableEvents() {
	// Without this a write to a closed pipe kills the process before the
	// model server is reaped; with it the write returns EPIPE instead.
	signal.Ignore(syscall.SIGPIPE)
	events = &eventStream{enc: json.NewEncoder(os.Stdout), gone: make(chan struct{})}
}

func emit(kind string, fields map[string]any) {
	if events == nil {
		return
	}
	line := map[string]any{"event": kind}
	for key, value := range fields {
		line[key] = value
	}
	events.mu.Lock()
	defer events.mu.Unlock()
	if events.closed {
		return
	}
	if err := events.enc.Encode(line); err != nil {
		events.closed = true
		close(events.gone)
	}
}

// Closed once nothing is reading stdout any more. Nil-safe: without
// `--events` it never fires.
func supervisorGone() <-chan struct{} {
	if events == nil {
		return nil
	}
	return events.gone
}

func heartbeat(done <-chan struct{}) {
	if events == nil {
		return
	}
	tick := time.NewTicker(heartbeatEvery)
	defer tick.Stop()
	for {
		select {
		case <-done:
			return
		case <-events.gone:
			return
		case <-tick.C:
			emit("alive", nil)
		}
	}
}
