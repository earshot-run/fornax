package main

// Models from the Ollama registry. `fornax pull ollama:llama3.2[:tag]` reads
// the model's OCI manifest on registry.ollama.ai — each layer's digest is
// already the artifact's SHA-256 and its size is exact, so the pin comes
// straight from the manifest — saves it to ~/.fornax/custom.json and
// installs through the same verify path as every other model.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"
)

const ollamaRegistry = "https://registry.ollama.ai/v2"

// `ollama:name[:tag]`, `ollama:ns/name[:tag]`, or a pasted
// ollama.com/library/name[:tag] URL — all become (namespace, name, tag).
func parseOllamaRef(arg string) (ns, name, tag string, err error) {
	ref := arg
	if strings.HasPrefix(ref, "ollama:") {
		ref = strings.TrimPrefix(ref, "ollama:")
	} else if u := strings.TrimPrefix(ref, "https://"); u != ref || strings.HasPrefix(ref, "http://") {
		u = strings.TrimPrefix(u, "http://")
		parts := strings.SplitN(u, "/", 3)
		if len(parts) != 3 || parts[0] != "ollama.com" || parts[1] != "library" {
			return "", "", "", fmt.Errorf("could not parse %q — want an ollama.com/library/<name> URL", arg)
		}
		ref = parts[2]
	}
	if i := strings.LastIndex(ref, ":"); i != -1 {
		tag = ref[i+1:]
		ref = ref[:i]
	}
	parts := strings.Split(ref, "/")
	switch len(parts) {
	case 1:
		ns, name = "library", parts[0]
	case 2:
		ns, name = parts[0], parts[1]
	default:
		return "", "", "", fmt.Errorf("could not parse %q — want ollama:<name>[:<tag>] or ollama:<ns>/<name>[:<tag>]", arg)
	}
	if tag == "" {
		tag = "latest"
	}
	clean := func(s string) bool {
		return s != "" && s == strings.Trim(idClean.ReplaceAllString(strings.ToLower(s), "-"), "-")
	}
	if !clean(name) || !clean(ns) || !clean(tag) {
		return "", "", "", fmt.Errorf("bad model name in %q", arg)
	}
	return ns, name, tag, nil
}

type ollamaLayer struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
}

// The manifest digest (from the response header), the weights layer, and a
// projector layer when the model ships one.
func resolveOllama(ctx context.Context, ns, name, tag string) (digest string, model, projector *ollamaLayer, err error) {
	url := fmt.Sprintf("%s/%s/%s/manifests/%s", ollamaRegistry, ns, name, tag)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", nil, nil, err
	}
	req.Header.Set("Accept", "application/vnd.oci.image.manifest.v1+json")
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", nil, nil, fmt.Errorf("could not reach registry.ollama.ai: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		io.Copy(io.Discard, resp.Body)
		return "", nil, nil, fmt.Errorf("no ollama model %s:%s — check the name on ollama.com/library", name, tag)
	}
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		return "", nil, nil, fmt.Errorf("registry.ollama.ai answered HTTP %d for %s", resp.StatusCode, url)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxHTTPBody))
	if err != nil {
		return "", nil, nil, fmt.Errorf("could not read the manifest: %w", err)
	}
	var manifest struct {
		Layers []ollamaLayer `json:"layers"`
	}
	if err := json.Unmarshal(body, &manifest); err != nil {
		return "", nil, nil, fmt.Errorf("bad manifest from registry.ollama.ai: %w", err)
	}
	var models []*ollamaLayer
	for i := range manifest.Layers {
		l := &manifest.Layers[i]
		switch l.MediaType {
		case "application/vnd.ollama.image.model":
			models = append(models, l)
		case "application/vnd.ollama.image.projector":
			projector = l
		}
	}
	if len(models) == 0 {
		return "", nil, nil, fmt.Errorf("ollama:%s:%s has no model layer — not a GGUF model", name, tag)
	}
	if len(models) > 1 {
		return "", nil, nil, fmt.Errorf("ollama:%s:%s ships %d model layers — multi-file models are unsupported, pull it via hf: instead", name, tag, len(models))
	}
	// The manifest's own hash is its OCI digest — the immutable revision.
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:]), models[0], projector, nil
}

func ollamaBlobURL(ns, name, digest string) string {
	return fmt.Sprintf("%s/%s/%s/blobs/%s", ollamaRegistry, ns, name, digest)
}

func deriveOllamaID(name, tag string) string {
	id := "ollama-" + name
	if tag != "latest" {
		id += "-" + tag
	}
	return strings.Trim(idClean.ReplaceAllString(strings.ToLower(id), "-"), "-")
}

func ollamaFileName(ns, name, tag string) string {
	base := name
	if ns != "library" {
		base = ns + "-" + name
	}
	return strings.Trim(idClean.ReplaceAllString(strings.ToLower(base+"-"+tag), "-"), "-") + ".gguf"
}

// `fornax pull ollama:<name>[:<tag>] [--as name] [--kind text|vision|audio]`
func cmdPullOllama(ctx context.Context, args []string) error {
	set := flag.NewFlagSet("pull", flag.ExitOnError)
	as := set.String("as", "", "custom id to save the model under")
	kind := set.String("kind", "", "text | vision | audio (default: text, or vision when the model ships a projector)")
	set.Usage = func() {
		fmt.Fprintln(os.Stderr, `usage: fornax pull ollama:<name>[:<tag>] [--as name]
       (or paste an ollama.com/library/<name> URL)`)
	}
	set.Parse(args)
	if set.NArg() != 1 {
		return fmt.Errorf("usage: fornax pull ollama:<name>[:<tag>] [--as name] [--kind vision]")
	}
	ns, name, tag, err := parseOllamaRef(set.Arg(0))
	if err != nil {
		return err
	}
	root := home()
	store, err := loadCustoms(root)
	if err != nil {
		return err
	}
	fetching := spin(fmt.Sprintf("resolving ollama:%s:%s", name, tag))
	manifestDigest, modelLayer, projLayer, err := resolveOllama(ctx, ns, name, tag)
	if err != nil {
		fetching.stop("")
		return err
	}
	kindName := *kind
	if kindName == "" {
		kindName = "text"
		if projLayer != nil {
			kindName = "vision"
		}
	}
	if kindName != "text" && kindName != "vision" && kindName != "audio" {
		fetching.stop("")
		return fmt.Errorf("--kind must be text, vision or audio")
	}
	if (kindName == "vision" || kindName == "audio") && projLayer == nil {
		fetching.stop("")
		return fmt.Errorf("ollama:%s:%s has no projector layer — --kind %s does not apply", name, tag, kindName)
	}
	id := *as
	if id == "" {
		id = deriveOllamaID(name, tag)
	}
	if id != strings.Trim(idClean.ReplaceAllString(strings.ToLower(id), "-"), "-") {
		fetching.stop("")
		return fmt.Errorf("--as %q is not a clean id — use lowercase letters, digits, dashes", *as)
	}
	if model(id) != nil || customSpec(root, id) != nil {
		fetching.stop("")
		return fmt.Errorf("%q is taken — pick another with --as (or `fornax rm %s` first)", id, id)
	}
	entry := customEntry{
		ID: id, Kind: kindName,
		Repo:     fmt.Sprintf("ollama:%s/%s:%s", ns, name, tag),
		Revision: manifestDigest,
		File:     ollamaFileName(ns, name, tag),
		Bytes:    modelLayer.Size,
		SHA256:   strings.TrimPrefix(modelLayer.Digest, "sha256:"),
		URL:      ollamaBlobURL(ns, name, modelLayer.Digest),
	}
	if projLayer != nil && kindName != "text" {
		entry.MMProj = &struct {
			File   string `json:"file"`
			Bytes  int64  `json:"bytes"`
			SHA256 string `json:"sha256"`
			URL    string `json:"url,omitempty"`
		}{
			File:   "mmproj-" + entry.File,
			Bytes:  projLayer.Size,
			SHA256: strings.TrimPrefix(projLayer.Digest, "sha256:"),
			URL:    ollamaBlobURL(ns, name, projLayer.Digest),
		}
	}
	fetching.stop("")
	eng := engine()
	if eng == nil {
		return fmt.Errorf("fornax does not have a pinned llama.cpp for %s/%s yet", runtime.GOOS, runtime.GOARCH)
	}
	if err := pull(ctx, entry.spec(), eng); err != nil {
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
