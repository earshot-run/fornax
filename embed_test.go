package main

// Live check for the embed path — gated on FORNAX_LIVE=1 since it pulls
// weights and spawns a real llama-server.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"testing"

	"github.com/earshot-run/fornax/internal/modelrt"
)

func TestEmbedLive(t *testing.T) {
	if os.Getenv("FORNAX_LIVE") == "" {
		t.Skip("set FORNAX_LIVE=1 to pull nomic-embed and spawn a real server")
	}
	ctx := context.Background()
	spec, _, err := modelrt.Resolve(ctx, "hf:nomic-ai/nomic-embed-text-v1.5-GGUF")
	if err != nil {
		t.Fatal(err)
	}

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	err = cmdEmbed(ctx, []string{spec.ID, "hello world"})
	w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Model      string    `json:"model"`
		Dimensions int       `json:"dimensions"`
		Embedding  []float64 `json:"embedding"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &parsed); err != nil {
		t.Fatalf("stdout is not clean JSON: %v\n%s", err, out)
	}
	if parsed.Model != spec.ID {
		t.Fatalf("model = %q", parsed.Model)
	}
	if parsed.Dimensions != 768 || len(parsed.Embedding) != 768 {
		t.Fatalf("dimensions=%d len(embedding)=%d, want 768", parsed.Dimensions, len(parsed.Embedding))
	}
}

// What `test nomic-embed` and `bench nomic-embed` print once wired in.
func TestEmbedTestBenchLive(t *testing.T) {
	if os.Getenv("FORNAX_LIVE") == "" {
		t.Skip("set FORNAX_LIVE=1 to pull nomic-embed and spawn a real server")
	}
	ctx := context.Background()
	spec, eng, err := modelrt.Resolve(ctx, "hf:nomic-ai/nomic-embed-text-v1.5-GGUF")
	if err != nil {
		t.Fatal(err)
	}
	if err := runEmbedTest(ctx, spec, eng); err != nil {
		t.Fatal(err)
	}
	if err := runEmbedBench(ctx, spec, eng, 3); err != nil {
		t.Fatal(err)
	}
}

// `ask` on an embed spec reaches chat completions on an embeddings-only
// server — observe the failure until dispatch guards land in main.go.
func TestAskEmbedLive(t *testing.T) {
	if os.Getenv("FORNAX_LIVE") == "" {
		t.Skip("set FORNAX_LIVE=1 to pull nomic-embed and spawn a real server")
	}
	err := cmdAsk(context.Background(), []string{"hf:nomic-ai/nomic-embed-text-v1.5-GGUF", "hi"})
	t.Logf("cmdAsk(nomic-embed) → %v", err)
	if err == nil {
		t.Fatal("ask on an embed model should fail")
	}
}
