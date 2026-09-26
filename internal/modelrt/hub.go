package modelrt

// Finding models before pulling one: the Hugging Face search behind
// `fornax search` and the studio's Models page, and a preview of what
// `pull hf:<ref>` would fetch — the same file, projector and split parts
// ensureHF would pick, with their sizes — without downloading anything.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/earshot-run/fornax/internal/openai"
	"github.com/earshot-run/fornax/internal/paths"
)

// One GGUF repo from the Hugging Face search.
type SearchHit struct {
	Repo      string   `json:"repo"`
	Downloads int64    `json:"downloads"`
	Likes     int64    `json:"likes"`
	GGUFs     []string `json:"ggufs"`
}

// GGUF repos matching query, most downloaded first, at most limit.
func SearchHF(ctx context.Context, query string, limit int) ([]SearchHit, error) {
	api := HFHost + "/api/models?search=" + url.QueryEscape(query) + "&filter=gguf&limit=40&full=false"
	req, err := hfRequest(ctx, http.MethodGet, api)
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("could not reach huggingface.co: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		return nil, fmt.Errorf("huggingface.co answered HTTP %d for the model search", resp.StatusCode)
	}
	var raw []struct {
		ID        string `json:"id"`
		Downloads int64  `json:"downloads"`
		Likes     int64  `json:"likes"`
		Siblings  []struct {
			RFilename string `json:"rfilename"`
		} `json:"siblings"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, openai.MaxBody)).Decode(&raw); err != nil {
		return nil, fmt.Errorf("the search reply was not readable: %w", err)
	}
	hits := make([]SearchHit, 0, len(raw))
	for _, r := range raw {
		hit := SearchHit{Repo: r.ID, Downloads: r.Downloads, Likes: r.Likes, GGUFs: []string{}}
		for _, s := range r.Siblings {
			if strings.HasSuffix(s.RFilename, ".gguf") {
				hit.GGUFs = append(hit.GGUFs, s.RFilename)
			}
		}
		hits = append(hits, hit)
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].Downloads > hits[j].Downloads })
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits, nil
}

// What pulling a hf: ref would fetch.
type Preview struct {
	Repo   string `json:"repo"`
	File   string `json:"file"`
	MMProj string `json:"mmproj,omitempty"`
	Kind   string `json:"kind"`
	// Weights, projector and split parts together.
	Bytes int64 `json:"bytes"`
}

// Resolves ref the way ensureHF does, sizes included, and saves nothing.
func PreviewHF(ctx context.Context, ref string) (*Preview, error) {
	repo, revision, file, err := parseHFRef(ref, "")
	if err != nil {
		return nil, err
	}
	files, err := repoFiles(ctx, repo)
	if err != nil {
		return nil, err
	}
	if file == "" {
		if file, err = pickGGUFFile(repo, files); err != nil {
			return nil, err
		}
	}
	p := &Preview{Repo: repo, File: file, Kind: inferKind(repo, file, findMMProj(files, file) != "")}
	fetch := []string{file}
	if p.Kind == "vision" || p.Kind == "audio" || p.Kind == "speech" {
		if p.MMProj = findMMProj(files, file); p.MMProj != "" {
			fetch = append(fetch, p.MMProj)
		}
	}
	fetch = append(fetch, splitCompanions(files, file)...)
	for _, f := range fetch {
		size, err := hfFileSize(ctx, repo, revision, f)
		if err != nil {
			return nil, err
		}
		p.Bytes += size
	}
	return p, nil
}

// A request to the Hugging Face API carrying the user's token, when there
// is one, to huggingface.co only.
func hfRequest(ctx context.Context, method, url string) (*http.Request, error) {
	url, err := paths.HFMirrorURL(url)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		return nil, err
	}
	if token := paths.HFToken(); token != "" && paths.IsHFHost(req.URL.Host) {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return req, nil
}

func gatedError(repo string) error {
	if paths.HFToken() == "" {
		return fmt.Errorf("%s is gated or private — accept its terms on huggingface.co/%s, then add a Hugging Face token (HF_TOKEN, or Models ▸ Hugging Face token in the studio)", repo, repo)
	}
	return fmt.Errorf("%s is gated or private and your Hugging Face token was refused — accept its terms on huggingface.co/%s with that account", repo, repo)
}

// The account a Hugging Face token belongs to; an error when the token is
// refused.
func HFWhoAmI(ctx context.Context, token string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, HFHost+"/api/whoami-v2", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return "", fmt.Errorf("could not reach huggingface.co: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		io.Copy(io.Discard, resp.Body)
		return "", fmt.Errorf("Hugging Face did not accept that token")
	}
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		return "", fmt.Errorf("huggingface.co answered HTTP %d", resp.StatusCode)
	}
	var who struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&who); err != nil {
		return "", err
	}
	return who.Name, nil
}
