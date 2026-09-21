package main

// Reranking — a llama-served model that speaks /v1/rerank (Jina-compatible)
// instead of chat completions. `fornax rerank` prints "score  doc" rows on
// stdout so scripts can pipe them; the doc text always comes from the input
// since llama-server omits document.text unless asked.

// NOTE for the integrator: the rerank endpoint is off by default —
// spawnServer (serve.go) needs `--reranking` for spec.kind == modalRerank,
// the way modalEmbed gets `--embeddings`. Verify against the pinned b11060
// build whether `--reranking` alone suffices or `--pooling rank` is also
// required (older llama.cpp wanted `--embedding --pooling rank`).

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
)

func init() {
	models = append(models, modelSpec{
		id:      "qwen3-rerank-0.6b",
		name:    "Qwen3 Reranker 0.6B",
		summary: "Scores documents against a query — for retrieval ordering.",
		kind:    modalRerank,
		repo:    "ggml-org/Qwen3-Reranker-0.6B-Q8_0-GGUF",
		model: filePin{
			file:     "qwen3-reranker-0.6b-q8_0.gguf",
			revision: "a02f48bb4f057028298c21fa033da2b30d7742d5",
			bytes:    639153184,
			sha256:   "22c9979ce4fbcdc5acdc310c6641c32797eff1aa980b8f7a2db8a8ea23429a48",
		},
		port: 7364,
	})
}

// One scored document, in the server's input indexing.
type rerankHit struct {
	index int
	score float64
	text  string
}

// `fornax rerank <model> "query" <doc…>` — or docs piped on stdin, one per
// line. Model and query are peeled off the front like cmdSay so flags can
// sit between them and the docs.
func cmdRerank(ctx context.Context, args []string) error {
	set := flag.NewFlagSet("rerank", flag.ExitOnError)
	top := set.Int("n", 0, "keep only the top N matches")
	usageLine := `usage: fornax rerank <model> "query" <doc…> [-n top]   (or pipe docs on stdin, one per line)`
	set.Usage = func() { fmt.Fprintln(os.Stderr, usageLine) }
	var id, query string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		id, args = args[0], args[1:]
	}
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		query, args = args[0], args[1:]
	}
	set.Parse(args)
	docs := set.Args()
	if id == "" && len(docs) > 0 {
		id, docs = docs[0], docs[1:]
	}
	if query == "" && len(docs) > 0 {
		query, docs = docs[0], docs[1:]
	}
	if id == "" || query == "" {
		return fmt.Errorf("%s", usageLine)
	}
	if len(docs) == 0 && !isTTY(os.Stdin) {
		scanner := bufio.NewScanner(os.Stdin)
		scanner.Buffer(make([]byte, 0, 64*1024), maxHTTPBody)
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
	spec, eng, err := resolve(id)
	if err != nil {
		return err
	}
	if spec.kind != modalRerank {
		return fmt.Errorf("%s does not rerank — pick a rerank model (`list`)", spec.id)
	}
	return withServer(ctx, spec, eng, func(url, key string) error {
		hits, err := rerankOnce(ctx, url, key, spec.id, query, docs, *top)
		if err != nil {
			return err
		}
		for _, hit := range hits {
			fmt.Printf("%.3f  %s\n", hit.score, shortDoc(hit.text))
		}
		fmt.Fprintf(os.Stderr, "%s\n", dim(fmt.Sprintf("%d docs, best match #%d", len(docs), hits[0].index+1)))
		return nil
	})
}

// One rerank call against POST /v1/rerank; hits come back sorted by score,
// best first. The server reports only an index + score unless return_text is
// set, so the row text falls back to the input doc by index.
func rerankOnce(ctx context.Context, url, key, model, query string, docs []string, topN int) ([]rerankHit, error) {
	req := map[string]any{
		"model":     model,
		"query":     query,
		"documents": docs,
	}
	if topN > 0 {
		req["top_n"] = topN
	}
	body, _ := json.Marshal(req)
	resp, err := post(ctx, url+"/rerank", key, body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, apiError(resp)
	}
	var parsed struct {
		Results []struct {
			Index          int     `json:"index"`
			RelevanceScore float64 `json:"relevance_score"`
			Document       *struct {
				Text string `json:"text"`
			} `json:"document"`
		} `json:"results"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxHTTPBody)).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("the reply was not readable: %w", err)
	}
	if len(parsed.Results) == 0 {
		return nil, fmt.Errorf("the server returned no rankings")
	}
	hits := make([]rerankHit, 0, len(parsed.Results))
	for _, r := range parsed.Results {
		hit := rerankHit{index: r.Index, score: r.RelevanceScore}
		if r.Document != nil && r.Document.Text != "" {
			hit.text = r.Document.Text
		} else {
			if r.Index < 0 || r.Index >= len(docs) {
				return nil, fmt.Errorf("the server returned a result index out of range")
			}
			hit.text = docs[r.Index]
		}
		hits = append(hits, hit)
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].score > hits[j].score })
	return hits, nil
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
func runRerankTest(ctx context.Context, spec *modelSpec, eng *engineSpec) error {
	return withServer(ctx, spec, eng, func(url, key string) error {
		hits, err := rerankOnce(ctx, url, key, spec.id,
			"what did the cat do",
			[]string{"the cat sat", "quantum physics"}, 0)
		if err != nil {
			return err
		}
		fmt.Printf("%s %s — best match scored %.2f\n", green("✓"), bold(spec.id), hits[0].score)
		return nil
	})
}
