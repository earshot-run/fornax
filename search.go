package main

// `fornax search <query…>` — the Hugging Face model API filtered to GGUF
// repos, ranked by downloads. A repo with exactly one .gguf file prints the
// ready `fornax pull hf:…` command; multi-file repos point at the file tree.

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/earshot-run/fornax/internal/modelrt"
	"github.com/earshot-run/fornax/internal/ui"
)

const hfSearchLimit = 10

// Download counts rendered short: 1_234_567 → 1.2M.
func humanCount(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fK", float64(n)/1_000)
	}
	return fmt.Sprintf("%d", n)
}

func cmdSearch(ctx context.Context, args []string) error {
	query := strings.TrimSpace(strings.Join(args, " "))
	if query == "" {
		return fmt.Errorf("usage: fornax search <query…>")
	}
	hits, err := modelrt.SearchHF(ctx, query, hfSearchLimit)
	if err != nil {
		return err
	}
	if len(hits) == 0 {
		fmt.Printf("no GGUF repos matching %q\n", query)
		return nil
	}
	fmt.Fprintf(os.Stderr, "%s\n", ui.Dim(fmt.Sprintf("%d GGUF repos for %q, by downloads", len(hits), query)))
	width := 0
	for i := range hits {
		if len(hits[i].Repo) > width {
			width = len(hits[i].Repo)
		}
	}
	for i := range hits {
		hit := &hits[i]
		kind := ui.Dim("no .gguf files listed")
		if n := len(hit.GGUFs); n > 0 {
			kind = fmt.Sprintf("%d .gguf", n)
		}
		fmt.Printf("  %s %s %s\n",
			ui.Cell(hit.Repo, width, ui.Bold), ui.Cell(humanCount(hit.Downloads)+" ↓", 9, nil), kind)
		switch len(hit.GGUFs) {
		case 1:
			fmt.Printf("  %s %s\n", ui.Cell("", width, nil),
				ui.Green("fornax pull hf:"+hit.Repo+"/"+hit.GGUFs[0]))
		case 0:
		default:
			fmt.Printf("  %s %s\n", ui.Cell("", width, nil),
				ui.Dim("pick a file: huggingface.co/"+hit.Repo+"/tree/main"))
		}
	}
	return nil
}
