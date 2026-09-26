package main

import (
	"context"
	"strings"
	"testing"
)

// Non-image models are refused before anything is fetched.
func TestDrawRejectsTextModel(t *testing.T) {
	err := cmdImagine(context.Background(), []string{"apple-fm", "a door"})
	if err == nil || !strings.Contains(err.Error(), "does not imagine") {
		t.Fatalf("expected a does-not-imagine error, got %v", err)
	}
}

func TestDrawUnknownModel(t *testing.T) {
	err := cmdImagine(context.Background(), []string{"nope-0b", "a door"})
	if err == nil || !strings.Contains(err.Error(), "unknown model") {
		t.Fatalf("expected an unknown-model error, got %v", err)
	}
}

func TestDrawNeedsModelAndPrompt(t *testing.T) {
	for _, args := range [][]string{
		{},
		{"some-model"},
	} {
		if err := cmdImagine(context.Background(), args); err == nil {
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
