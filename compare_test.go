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
		[]string{"hf:Qwen/Qwen3-4B-GGUF,apple-fm", "name one primary color"})
	if err != nil {
		t.Fatal(err)
	}
}

// kev models must be refused before anything loads — the first model
// resolves through a saved custom entry so nothing touches the network.
func TestCompareRejectsKev(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FORNAX_HOME", root)
	if err := saveCustoms(root, &customStore{Models: []customEntry{{
		ID: "t1", Kind: "text", Repo: "o/r", Revision: "abc123",
		File: "f.gguf", Bytes: 1, SHA256: "0",
	}}}); err != nil {
		t.Fatal(err)
	}
	err := cmdCompare(context.Background(),
		[]string{"t1,kev-4b", "name one primary color"})
	if err == nil {
		t.Fatal("expected a refusal for kev models")
	}
}
