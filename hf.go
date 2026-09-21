package main

// User-added models from Hugging Face. `fornax pull hf:Org/Repo/File.gguf`
// (or a pasted huggingface.co URL) resolves the pin at fetch time — HF serves
// the LFS sha256 in `x-linked-etag`, the byte count in `x-linked-size` and the
// immutable commit in `x-repo-commit` — saves it to ~/.fornax/custom.json, and
// installs through the same verify/receipt path as catalog models.

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const customFile = "custom.json"

// A saved custom model. Serialized — the catalog's modelSpec isn't.
type customEntry struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"` // text | vision | audio
	Repo     string `json:"repo"`
	Revision string `json:"revision"`
	File     string `json:"file"`
	Bytes    int64  `json:"bytes"`
	SHA256   string `json:"sha256"`
	// Direct download URL for non-Hugging-Face sources (ollama registry
	// blobs). Empty = built from repo/revision/file on huggingface.co.
	URL    string `json:"url,omitempty"`
	MMProj *struct {
		File   string `json:"file"`
		Bytes  int64  `json:"bytes"`
		SHA256 string `json:"sha256"`
		URL    string `json:"url,omitempty"`
	} `json:"mmproj,omitempty"`
	Port int `json:"port"`
}

type customStore struct {
	Version int           `json:"version"`
	Models  []customEntry `json:"models"`
}

func loadCustoms(root string) (*customStore, error) {
	data, err := os.ReadFile(filepath.Join(root, customFile))
	if err != nil {
		if os.IsNotExist(err) {
			return &customStore{Version: 1}, nil
		}
		return nil, err
	}
	var store customStore
	if err := json.Unmarshal(data, &store); err != nil {
		return nil, fmt.Errorf("%s is unreadable — delete it to reset custom models", filepath.Join(root, customFile))
	}
	return &store, nil
}

func saveCustoms(root string, store *customStore) error {
	store.Version = 1
	return atomicJSONPrivate(filepath.Join(root, customFile), store)
}

// The catalog entries plus every saved custom model, as specs.
func allSpecs(root string) []*modelSpec {
	specs := make([]*modelSpec, 0, len(models)+4)
	for i := range models {
		specs = append(specs, &models[i])
	}
	if store, err := loadCustoms(root); err == nil {
		for i := range store.Models {
			specs = append(specs, store.Models[i].spec())
		}
	}
	return specs
}

func (e *customEntry) spec() *modelSpec {
	kind := modalText
	switch e.Kind {
	case "vision":
		kind = modalVision
	case "audio":
		kind = modalAudio
	}
	spec := &modelSpec{
		id:   e.ID,
		name: e.ID,
		kind: kind,
		repo: e.Repo,
		model: filePin{
			file:     e.File,
			revision: e.Revision,
			bytes:    e.Bytes,
			sha256:   e.SHA256,
			url:      e.URL,
		},
		port: e.Port,
	}
	spec.summary = fmt.Sprintf("custom — %s @ %.7s", e.Repo+"/"+e.File, e.Revision)
	if e.MMProj != nil {
		spec.mmproj = &filePin{file: e.MMProj.File, revision: e.Revision, bytes: e.MMProj.Bytes, sha256: e.MMProj.SHA256, url: e.MMProj.URL}
	}
	return spec
}

func customSpec(root, id string) *modelSpec {
	store, err := loadCustoms(root)
	if err != nil {
		return nil
	}
	for i := range store.Models {
		if store.Models[i].ID == id {
			return store.Models[i].spec()
		}
	}
	return nil
}

func dropCustom(root, id string) error {
	store, err := loadCustoms(root)
	if err != nil {
		return err
	}
	kept := store.Models[:0]
	for _, m := range store.Models {
		if m.ID != id {
			kept = append(kept, m)
		}
	}
	store.Models = kept
	return saveCustoms(root, store)
}

// `hf:Org/Repo/File[@rev]`, `hf.co/…`, or a pasted huggingface.co
// blob/resolve URL — all become (repo, rev, file).
func parseHFRef(arg, revFlag string) (repo, rev, file string, err error) {
	rev = revFlag
	ref := arg
	if strings.HasPrefix(ref, "hf:") {
		ref = strings.TrimPrefix(ref, "hf:")
	} else if strings.HasPrefix(ref, "hf.co/") {
		ref = "https://huggingface.co/" + strings.TrimPrefix(ref, "hf.co/")
	}
	if strings.HasPrefix(ref, "https://huggingface.co/") || strings.HasPrefix(ref, "http://huggingface.co/") {
		path := strings.TrimPrefix(strings.TrimPrefix(strings.TrimPrefix(ref, "https://huggingface.co/"), "http://huggingface.co/"), "/")
		parts := strings.SplitN(path, "/", 4)
		if len(parts) != 4 || (parts[2] != "blob" && parts[2] != "resolve") {
			return "", "", "", fmt.Errorf("could not parse %q — want a huggingface.co …/blob|resolve/<rev>/<file> URL", arg)
		}
		repo = parts[0] + "/" + parts[1]
		rest := strings.SplitN(parts[3], "/", 2)
		if len(rest) != 2 {
			return "", "", "", fmt.Errorf("could not parse %q — missing file path", arg)
		}
		if revFlag == "" {
			rev = rest[0]
		}
		file = rest[1]
	} else {
		if at := strings.LastIndex(ref, "@"); at != -1 {
			if revFlag == "" {
				rev = ref[at+1:]
			}
			ref = ref[:at]
		}
		parts := strings.SplitN(ref, "/", 3)
		if len(parts) != 3 {
			return "", "", "", fmt.Errorf("could not parse %q — want hf:Org/Repo/File.gguf[@rev]", arg)
		}
		repo, file = parts[0]+"/"+parts[1], parts[2]
	}
	if rev == "" {
		rev = "main"
	}
	if file == "" || strings.Contains(file, "..") {
		return "", "", "", fmt.Errorf("bad file in %q", arg)
	}
	return repo, rev, file, nil
}

// HEAD the resolve URL: x-linked-etag is the LFS sha256, x-linked-size the
// bytes, x-repo-commit the immutable revision to pin.
func resolveHFPin(ctx context.Context, repo, rev, file string) (*filePin, error) {
	url := fmt.Sprintf("https://huggingface.co/%s/resolve/%s/%s", repo, rev, file)
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return nil, err
	}
	// The pin headers live on huggingface.co's own response — following the
	// CDN redirect would drop them.
	client := &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("could not reach huggingface.co: %w", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("%s is gated or private — fornax only pulls public files", url)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		return nil, fmt.Errorf("huggingface.co answered HTTP %d for %s", resp.StatusCode, url)
	}
	size, _ := strconv.ParseInt(resp.Header.Get("x-linked-size"), 10, 64)
	sha := strings.Trim(resp.Header.Get("x-linked-etag"), `"`)
	commit := resp.Header.Get("x-repo-commit")
	if sha == "" || size == 0 {
		return nil, fmt.Errorf("%s is not an LFS file — fornax can only pin LFS artifacts", file)
	}
	if commit == "" {
		commit = rev
	}
	return &filePin{file: file, revision: commit, bytes: size, sha256: sha}, nil
}

var idClean = regexp.MustCompile(`[^a-z0-9]+`)

// A short, safe id derived from the filename: Qwen3-0.6B-Q8_0.gguf →
// hf-qwen3-0.6b-q8-0.
func deriveHFID(file string) string {
	base := filepath.Base(file)
	base = strings.TrimSuffix(base, filepath.Ext(base))
	id := "hf-" + strings.Trim(idClean.ReplaceAllString(strings.ToLower(base), "-"), "-")
	return id
}

func nextCustomPort(store *customStore) int {
	used := map[int]bool{}
	for i := range models {
		used[models[i].port] = true
	}
	for _, m := range store.Models {
		used[m.Port] = true
	}
	for p := 7401; p < scratchPortBase; p++ {
		if !used[p] {
			return p
		}
	}
	return 0
}

// `fornax pull hf:Org/Repo/File.gguf [--as name] [--kind text|vision|audio]
// [--mmproj File] [--rev sha|branch]`
func cmdPullHF(ctx context.Context, args []string) error {
	set := flag.NewFlagSet("pull", flag.ExitOnError)
	as := set.String("as", "", "custom id to save the model under")
	kind := set.String("kind", "text", "text | vision | audio")
	mmproj := set.String("mmproj", "", "projector file in the same repo (required for vision/audio)")
	rev := set.String("rev", "", "branch, tag or commit (default: main)")
	set.Usage = func() {
		fmt.Fprintln(os.Stderr, `usage: fornax pull hf:Org/Repo/File.gguf [--as name] [--kind vision --mmproj projector.gguf]
       (or paste a huggingface.co blob/resolve URL)`)
	}
	set.Parse(args)
	if set.NArg() != 1 {
		return fmt.Errorf("usage: fornax pull hf:Org/Repo/File.gguf [--as name] [--kind vision|audio --mmproj F]")
	}
	repo, revision, file, err := parseHFRef(set.Arg(0), *rev)
	if err != nil {
		return err
	}
	if *kind != "text" && *kind != "vision" && *kind != "audio" {
		return fmt.Errorf("--kind must be text, vision or audio")
	}
	if (*kind == "vision" || *kind == "audio") && *mmproj == "" {
		return fmt.Errorf("--kind %s needs --mmproj <projector file in %s>", *kind, repo)
	}
	root := home()
	store, err := loadCustoms(root)
	if err != nil {
		return err
	}
	id := *as
	if id == "" {
		id = deriveHFID(file)
	}
	if id != strings.Trim(idClean.ReplaceAllString(strings.ToLower(id), "-"), "-") {
		return fmt.Errorf("--as %q is not a clean id — use lowercase letters, digits, dashes", *as)
	}
	if model(id) != nil || customSpec(root, id) != nil {
		return fmt.Errorf("%q is taken — pick another with --as (or `fornax rm %s` first)", id, id)
	}
	fetching := spin("resolving " + repo + "/" + file)
	pin, err := resolveHFPin(ctx, repo, revision, file)
	if err != nil {
		fetching.stop("")
		return err
	}
	entry := customEntry{
		ID: id, Kind: *kind, Repo: repo, Revision: pin.revision,
		File: file, Bytes: pin.bytes, SHA256: pin.sha256,
	}
	if *mmproj != "" {
		proj, err := resolveHFPin(ctx, repo, revision, *mmproj)
		if err != nil {
			fetching.stop("")
			return err
		}
		entry.MMProj = &struct {
			File   string `json:"file"`
			Bytes  int64  `json:"bytes"`
			SHA256 string `json:"sha256"`
			URL    string `json:"url,omitempty"`
		}{File: *mmproj, Bytes: proj.bytes, SHA256: proj.sha256}
	}
	fetching.stop("")
	spec := entry.spec()
	eng := engine()
	if eng == nil {
		return fmt.Errorf("fornax does not have a pinned llama.cpp for %s/%s yet", runtime.GOOS, runtime.GOARCH)
	}
	if err := pull(ctx, spec, eng); err != nil {
		return err
	}
	entry.Port = nextCustomPort(store)
	store.Models = append(store.Models, entry)
	if err := saveCustoms(root, store); err != nil {
		return fmt.Errorf("model installed but could not save %s: %w", customFile, err)
	}
	fmt.Fprintf(os.Stderr, "%s saved as %s — `fornax ask %s …` / `fornax run %s`\n",
		green("✓"), bold(id), id, id)
	return nil
}
