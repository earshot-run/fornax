package main

import (
	"context"
	"os"
	"testing"
)

// Live check against installed models — run with FORNAX_LIVE=1.
func TestCompareLive(t *testing.T) {
	if os.Getenv("FORNAX_LIVE") == "" {
		t.Skip("set FORNAX_LIVE=1 to run against real models")
	}
	err := cmdCompare(context.Background(),
		[]string{"qwen3-4b,apple-fm", "name one primary color"})
	if err != nil {
		t.Fatal(err)
	}
}

// kev models must be refused before anything loads.
func TestCompareRejectsKev(t *testing.T) {
	err := cmdCompare(context.Background(),
		[]string{"qwen3-4b,kev-4b", "name one primary color"})
	if err == nil {
		t.Fatal("expected a refusal for kev models")
	}
}
