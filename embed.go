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
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/earshot-run/fornax/internal/catalog"
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
	return withServer(ctx, spec, eng, func(url, key string) error {
		vec, err := embedOnce(ctx, url, key, spec.ID, text)
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

// One embeddings call; returns the pooled vector.
func embedOnce(ctx context.Context, url, key, model, text string) ([]float64, error) {
	body, _ := json.Marshal(map[string]any{
		"model": model,
		"input": text,
	})
	resp, err := openai.Post(ctx, url+"/embeddings", key, body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, openai.Error(resp)
	}
	var parsed struct {
		Data []struct {
			Embedding json.RawMessage `json:"embedding"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, openai.MaxBody)).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("the reply was not readable: %w", err)
	}
	if len(parsed.Data) == 0 {
		return nil, fmt.Errorf("the server returned no embedding")
	}
	var vec []float64
	if err := json.Unmarshal(parsed.Data[0].Embedding, &vec); err == nil {
		return vec, nil
	}
	// Some builds wrap the pooled vector in an extra array.
	var rows [][]float64
	if err := json.Unmarshal(parsed.Data[0].Embedding, &rows); err == nil && len(rows) == 1 {
		return rows[0], nil
	}
	return nil, fmt.Errorf("the server returned an embedding shape fornax cannot read")
}

func runEmbedTest(ctx context.Context, spec *catalog.Spec, eng *catalog.EngineSpec) error {
	return withServer(ctx, spec, eng, func(url, key string) error {
		started := time.Now()
		vec, err := embedOnce(ctx, url, key, spec.ID, "Reply with exactly: ok")
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
	return withServer(ctx, spec, eng, func(url, key string) error {
		var lat []float64
		for i := 0; i < runs; i++ {
			started := time.Now()
			if _, err := embedOnce(ctx, url, key, spec.ID, probe); err != nil {
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
