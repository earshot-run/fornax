package events

import (
	"encoding/json"
	"io"
	"os"
	"testing"
	"time"
)

// capture points os.Stdout at a pipe and returns the read end, with a
// cleanup that restores the real stdout and clears the package singleton.
func capture(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	stream = nil
	t.Cleanup(func() {
		os.Stdout = old
		stream = nil
		w.Close()
		r.Close()
	})
	return r, w
}

func TestEmitWritesOneJSONLine(t *testing.T) {
	if On() {
		t.Fatal("On() is true before Enable()")
	}
	r, w := capture(t)
	Enable()
	if !On() {
		t.Fatal("On() is false after Enable()")
	}
	Emit("progress", map[string]any{"done": 5, "total": 10})
	w.Close()

	line, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(line, &got); err != nil {
		t.Fatalf("not JSON: %v (%q)", err, line)
	}
	if got["event"] != "progress" || got["done"] != float64(5) || got["total"] != float64(10) {
		t.Fatalf("got %v", got)
	}
}

func TestEmitBeforeEnableIsSilent(t *testing.T) {
	capture(t)
	Emit("ready", nil) // must not panic or write
}

func TestSupervisorGoneClosesWhenStdoutBreaks(t *testing.T) {
	r, w := capture(t)
	Enable()
	r.Close() // the reader is gone; the next write fails
	Emit("alive", nil)

	select {
	case <-SupervisorGone():
	case <-time.After(2 * time.Second):
		t.Fatal("SupervisorGone did not close after a failed write")
	}
	w.Close()
}

func TestHeartbeatStopsWithDone(t *testing.T) {
	capture(t)
	Enable()
	done := make(chan struct{})
	close(done)
	stopped := make(chan struct{})
	go func() { Heartbeat(done); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("Heartbeat did not return when done closed")
	}
}

func TestSupervisorGoneNilWithoutEvents(t *testing.T) {
	stream = nil
	if SupervisorGone() != nil {
		t.Fatal("SupervisorGone is non-nil without Enable")
	}
}
