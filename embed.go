package main

// Embeddings — a llama-served model that speaks /v1/embeddings instead of
// chat completions. `fornax embed` prints the vector as JSON on stdout so
// scripts can pipe it; test/bench get their own shapes since a chat prompt
// proves nothing about an embed model.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/earshot-run/fornax/internal/catalog"
	"github.com/earshot-run/fornax/internal/modelrt"
	"github.com/earshot-run/fornax/internal/openai"
	"github.com/earshot-run/fornax/internal/ui"
)

func cmdEmbed(ctx context.Context, args []string) error {
	spec, eng, rest, err := modelArgs(ctx, "embed", args, " [text…]  (or pipe it on stdin)")
	if err != nil {
		return err
	}
	if spec.Kind != catalog.Embed {
		return fmt.Errorf("%s does not embed — pick an embed model (`list`)", spec.ID)
	}
	text := strings.Join(rest, " ")
	if text == "" {
		if ui.IsTTY(os.Stdin) {
			return fmt.Errorf("usage: fornax embed <model> <text…>  (or pipe text on stdin)")
		}
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return err
		}
		text = strings.TrimSpace(string(data))
	}
	if text == "" {
		return fmt.Errorf("usage: fornax embed <model> <text…>")
	}
	return modelrt.WithServer(ctx, spec, eng, func(url, key string) error {
		vec, err := openai.Embed(ctx, url, key, spec.ID, text)
		if err != nil {
			return err
		}
		out, _ := json.Marshal(struct {
			Model      string    `json:"model"`
			Dimensions int       `json:"dimensions"`
			Embedding  []float64 `json:"embedding"`
		}{spec.ID, len(vec), vec})
		fmt.Println(string(out))
		return nil
	})
}

func runEmbedTest(ctx context.Context, spec *catalog.Spec, eng *catalog.EngineSpec) error {
	return modelrt.WithServer(ctx, spec, eng, func(url, key string) error {
		started := time.Now()
		vec, err := openai.Embed(ctx, url, key, spec.ID, "Reply with exactly: ok")
		elapsed := time.Since(started)
		if err != nil {
			return err
		}
		fmt.Printf("%s %s — %d dims in %.1fs\n", ui.Green("✓"), ui.Bold(spec.ID), len(vec), elapsed.Seconds())
		return nil
	})
}

// Median request latency over a handful of identical embed calls.
func runEmbedBench(ctx context.Context, spec *catalog.Spec, eng *catalog.EngineSpec, runs int) error {
	const probe = "The quick brown fox jumps over the lazy dog."
	return modelrt.WithServer(ctx, spec, eng, func(url, key string) error {
		var lat []float64
		for i := 0; i < runs; i++ {
			started := time.Now()
			if _, err := openai.Embed(ctx, url, key, spec.ID, probe); err != nil {
				return err
			}
			ms := float64(time.Since(started)) / float64(time.Millisecond)
			lat = append(lat, ms)
			fmt.Printf("  %s %.0f ms\n", ui.Dim(fmt.Sprintf("run %d", i+1)), ms)
		}
		sort.Float64s(lat)
		fmt.Printf("%s %s — median %.0f ms over %d requests\n", ui.Green("✓"), ui.Bold(spec.ID), lat[len(lat)/2], len(lat))
		return nil
	})
}
