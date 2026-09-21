package main

// `fornax search <query…>` — the Hugging Face model API filtered to GGUF
// repos, ranked by downloads. A repo with exactly one .gguf file prints the
// ready `fornax pull hf:…` command; multi-file repos point at the file tree.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

const hfSearchLimit = 10

type hfSearchHit struct {
	ID        string `json:"id"`
	Downloads int64  `json:"downloads"`
	Siblings  []struct {
		RFilename string `json:"rfilename"`
	} `json:"siblings"`
}

func (h *hfSearchHit) ggufFiles() []string {
	var files []string
	for _, s := range h.Siblings {
		if strings.HasSuffix(s.RFilename, ".gguf") {
			files = append(files, s.RFilename)
		}
	}
	return files
}

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
	api := "https://huggingface.co/api/models?search=" + url.QueryEscape(query) +
		"&filter=gguf&limit=15&full=false"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, api, nil)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("could not reach huggingface.co: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		return fmt.Errorf("huggingface.co answered HTTP %d for the model search", resp.StatusCode)
	}
	var hits []hfSearchHit
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxHTTPBody)).Decode(&hits); err != nil {
		return fmt.Errorf("the search reply was not readable: %w", err)
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].Downloads > hits[j].Downloads })
	if len(hits) > hfSearchLimit {
		hits = hits[:hfSearchLimit]
	}
	if len(hits) == 0 {
		fmt.Printf("no GGUF repos matching %q\n", query)
		return nil
	}
	fmt.Fprintf(os.Stderr, "%s\n", dim(fmt.Sprintf("%d GGUF repos for %q, by downloads", len(hits), query)))
	width := 0
	for i := range hits {
		if len(hits[i].ID) > width {
			width = len(hits[i].ID)
		}
	}
	for i := range hits {
		hit := &hits[i]
		ggufs := hit.ggufFiles()
		kind := dim("no .gguf files listed")
		if n := len(ggufs); n > 0 {
			kind = fmt.Sprintf("%d .gguf", n)
		}
		fmt.Printf("  %s %s %s\n",
			cell(hit.ID, width, bold), cell(humanCount(hit.Downloads)+" ↓", 9, nil), kind)
		switch len(ggufs) {
		case 1:
			fmt.Printf("  %s %s\n", cell("", width, nil),
				green("fornax pull hf:"+hit.ID+"/"+ggufs[0]))
		case 0:
		default:
			fmt.Printf("  %s %s\n", cell("", width, nil),
				dim("pick a file: huggingface.co/"+hit.ID+"/tree/main"))
		}
	}
	return nil
}
