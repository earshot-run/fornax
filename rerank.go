package main

// Reranking — a llama-served model that speaks /v1/rerank (Jina-compatible)
// instead of chat completions. `fornax rerank` prints "score  doc" rows on
// stdout so scripts can pipe them; the doc text always comes from the input
// since llama-server omits document.text unless asked. The wire client lives
// in openai, shared with the MCP server.

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/earshot-run/fornax/internal/catalog"
	"github.com/earshot-run/fornax/internal/modelrt"
	"github.com/earshot-run/fornax/internal/openai"
	"github.com/earshot-run/fornax/internal/ui"
)

// `fornax rerank <model> "query" <doc…>` — or docs piped on stdin, one per
// line.
func cmdRerank(ctx context.Context, args []string) error {
	set := flag.NewFlagSet("rerank", flag.ExitOnError)
	top := set.Int("n", 0, "keep only the top N matches")
	usageLine := `usage: fornax rerank <model> "query" <doc…> [-n top]   (or pipe docs on stdin, one per line)`
	set.Usage = ui.UsageFunc(set, usageLine)
	got := parseFlexible(set, args, 2)
	if len(got) < 2 {
		return fmt.Errorf("%s", usageLine)
	}
	id, query, docs := got[0], got[1], got[2:]
	if len(docs) == 0 && !ui.IsTTY(os.Stdin) {
		scanner := bufio.NewScanner(os.Stdin)
		scanner.Buffer(make([]byte, 0, 64*1024), openai.MaxBody)
		for scanner.Scan() {
			if line := strings.TrimSpace(scanner.Text()); line != "" {
				docs = append(docs, line)
			}
		}
		if err := scanner.Err(); err != nil {
			return err
		}
	}
	if len(docs) == 0 {
		return fmt.Errorf("%s", usageLine)
	}
	spec, eng, err := modelrt.Resolve(ctx, id)
	if err != nil {
		return err
	}
	if spec.Kind != catalog.Rerank {
		return fmt.Errorf("%s does not rerank — pick a rerank model (`list`)", spec.ID)
	}
	return modelrt.WithServer(ctx, spec, eng, func(url, key string) error {
		hits, err := openai.Rerank(ctx, url, key, spec.ID, query, docs, *top)
		if err != nil {
			return err
		}
		for _, hit := range hits {
			fmt.Printf("%.3f  %s\n", hit.Score, shortDoc(hit.Text))
		}
		fmt.Fprintf(os.Stderr, "%s\n", ui.Dim(fmt.Sprintf("%d docs, best match #%d", len(docs), hits[0].Index+1)))
		return nil
	})
}

// One display line's worth of a doc: whitespace collapsed, ~80 chars max.
func shortDoc(doc string) string {
	doc = strings.Join(strings.Fields(doc), " ")
	runes := []rune(doc)
	if len(runes) > 80 {
		doc = string(runes[:79]) + "…"
	}
	return doc
}

// A rerank model's `test` scores two unrelated docs — the cat should win.
func runRerankTest(ctx context.Context, spec *catalog.Spec, eng *catalog.EngineSpec) error {
	return modelrt.WithServer(ctx, spec, eng, func(url, key string) error {
		hits, err := openai.Rerank(ctx, url, key, spec.ID,
			"what did the cat do",
			[]string{"the cat sat", "quantum physics"}, 0)
		if err != nil {
			return err
		}
		fmt.Printf("%s %s — best match scored %.2f\n", ui.Green("✓"), ui.Bold(spec.ID), hits[0].Score)
		return nil
	})
}
