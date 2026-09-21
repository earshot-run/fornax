package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Non-image models are refused before anything is fetched.
func TestDrawRejectsTextModel(t *testing.T) {
	err := cmdDraw(context.Background(), []string{"qwen3-1.7b", "a door"})
	if err == nil || !strings.Contains(err.Error(), "does not draw") {
		t.Fatalf("expected a does-not-draw error, got %v", err)
	}
}

func TestDrawUnknownModel(t *testing.T) {
	err := cmdDraw(context.Background(), []string{"nope-0b", "a door"})
	if err == nil || !strings.Contains(err.Error(), "unknown model") {
		t.Fatalf("expected an unknown-model error, got %v", err)
	}
}

func TestDrawNeedsModelAndPrompt(t *testing.T) {
	for _, args := range [][]string{
		{},
		{"sdxl-turbo"},
	} {
		if err := cmdDraw(context.Background(), args); err == nil {
			t.Fatalf("args %v: expected a usage error", args)
		}
	}
}

func TestDrawSizeParses(t *testing.T) {
	if w, h, err := drawSize("768x512"); err != nil || w != 768 || h != 512 {
		t.Fatalf("drawSize(768x512) = %d, %d, %v", w, h, err)
	}
	for _, bad := range []string{"", "512", "x512", "512x", "axb", "-1x512", "0x0", "8192x8192"} {
		if _, _, err := drawSize(bad); err == nil {
			t.Fatalf("drawSize(%q) should fail", bad)
		}
	}
}

func TestSDEnginePinIsComplete(t *testing.T) {
	eng := engineSD()
	if eng == nil {
		t.Skip("unsupported platform")
	}
	if len(eng.sha256) != 64 || eng.bytes <= 0 || eng.binary == "" {
		t.Fatal("incomplete sd engine pin")
	}
	if !strings.HasSuffix(eng.url, eng.archive) {
		t.Fatal("url does not end with the pinned archive")
	}
}

// The real end-to-end: downloads ~34MB of engine and ~7GB of weights, then
// draws. `FORNAX_LIVE_DRAW=1 go test -run TestDrawLive -timeout 30m`.
func TestDrawLive(t *testing.T) {
	if os.Getenv("FORNAX_LIVE_DRAW") == "" {
		t.Skip("set FORNAX_LIVE_DRAW=1 to fetch weights and draw for real")
	}
	out := filepath.Join(t.TempDir(), "doorbell.png")
	err := cmdDraw(context.Background(),
		[]string{"sdxl-turbo", "a tiny doorbell icon", "-o", out, "-steps", "2"})
	if err != nil {
		t.Fatal(err)
	}
	info, statErr := os.Stat(out)
	if statErr != nil || info.Size() < 10_000 {
		t.Fatalf("expected a real PNG at %s, stat err %v", out, statErr)
	}
}
