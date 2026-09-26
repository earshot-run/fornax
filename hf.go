package main

// User-added models from Hugging Face. `fornax pull hf:Org/Repo/File.gguf`
// (or a pasted huggingface.co URL) resolves the pin at fetch time — HF serves
// the LFS sha256 in `x-linked-etag`, the byte count in `x-linked-size` and the
// immutable commit in `x-repo-commit` — saves it to ~/.fornax/custom.json, and
// installs through the same verify/receipt path as the built-ins.

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
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/earshot-run/fornax/internal/catalog"
	"github.com/earshot-run/fornax/internal/openai"
	"github.com/earshot-run/fornax/internal/paths"
	"github.com/earshot-run/fornax/internal/ui"
)

const customFile = "custom.json"

// A saved custom model. Serialized — catalog.Spec isn't.
type customEntry struct {
	ID   string `json:"id"`
	Kind string `json:"kind"` // text | vision | audio | image | video | embed | rerank | speech
	// The reference it was pulled as — `hf:Org/Repo` resolves to the same
	// entry without touching the network again.
	Ref  string `json:"ref,omitempty"`
	Repo string `json:"repo"`
	// The ref's requested rev ("main" when unspecified) — Revision below is
	// the immutable commit it resolved to.
	Want     string `json:"want,omitempty"`
	Revision string `json:"revision"`
	File     string `json:"file"`
	Bytes    int64  `json:"bytes"`
	SHA256   string `json:"sha256"`
	// Direct download URL for non-Hugging-Face sources (ollama registry
	// blobs). Empty = built from repo/revision/file on huggingface.co.
	URL    string     `json:"url,omitempty"`
	MMProj *customPin `json:"mmproj,omitempty"`
	// image and video only: files passed to sd-cli under their own flag,
	// and the engine arguments the operator saved for this model.
	Companions []customCompanion `json:"companions,omitempty"`
	Args       []string          `json:"args,omitempty"`
	Port       int               `json:"port"`
}

type customPin struct {
	File   string `json:"file"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
	URL    string `json:"url,omitempty"`
}

type customCompanion struct {
	Flag   string `json:"flag"`
	File   string `json:"file"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
	URL    string `json:"url"`
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
	return paths.AtomicJSON(filepath.Join(root, customFile), store)
}

// The built-ins plus every saved custom model, as specs.
func allSpecs(root string) []*catalog.Spec {
	specs := catalog.Models()
	if store, err := loadCustoms(root); err == nil {
		for i := range store.Models {
			specs = append(specs, store.Models[i].spec())
		}
	}
	return specs
}

func (e *customEntry) spec() *catalog.Spec {
	kind := catalog.Text
	switch e.Kind {
	case "vision":
		kind = catalog.Vision
	case "audio":
		kind = catalog.Audio
	case "image":
		kind = catalog.Image
	case "video":
		kind = catalog.Video
	case "embed":
		kind = catalog.Embed
	case "rerank":
		kind = catalog.Rerank
	case "speech":
		kind = catalog.Speech
	}
	spec := &catalog.Spec{
		ID:   e.ID,
		Name: e.ID,
		Kind: kind,
		Repo: e.Repo,
		Model: catalog.Pin{
			File:     e.File,
			Revision: e.Revision,
			Bytes:    e.Bytes,
			SHA256:   e.SHA256,
			URL:      e.URL,
		},
		Port: e.Port,
	}
	spec.Summary = fmt.Sprintf("custom — %s @ %.7s", e.Repo+"/"+e.File, e.Revision)
	if e.MMProj != nil {
		spec.MMProj = &catalog.Pin{File: e.MMProj.File, Revision: e.Revision, Bytes: e.MMProj.Bytes, SHA256: e.MMProj.SHA256, URL: e.MMProj.URL}
	}
	if kind == catalog.Image || kind == catalog.Video {
		spec.Runtime = catalog.SD
		spec.Args = e.Args
	}
	for _, c := range e.Companions {
		spec.Companions = append(spec.Companions, catalog.Pin{Flag: c.Flag, File: c.File, Bytes: c.Bytes, SHA256: c.SHA256, URL: c.URL})
	}
	return spec
}

func customSpec(root, id string) *catalog.Spec {
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

// `hf:Org/Repo[/File][@rev]`, `hf.co/…`, or a pasted huggingface.co
// blob/resolve URL — all become (repo, rev, file). An empty file means
// "pick for me": the repo listing decides.
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
		if len(parts) < 2 {
			return "", "", "", fmt.Errorf("could not parse %q — want a huggingface.co/<org>/<repo> URL", arg)
		}
		repo = parts[0] + "/" + parts[1]
		if len(parts) == 4 {
			if parts[2] != "blob" && parts[2] != "resolve" {
				return "", "", "", fmt.Errorf("could not parse %q — want a huggingface.co …/blob|resolve/<rev>/<file> URL", arg)
			}
			rest := strings.SplitN(parts[3], "/", 2)
			if len(rest) != 2 {
				return "", "", "", fmt.Errorf("could not parse %q — missing file path", arg)
			}
			if revFlag == "" {
				rev = rest[0]
			}
			file = rest[1]
		}
	} else {
		if at := strings.LastIndex(ref, "@"); at != -1 {
			if revFlag == "" {
				rev = ref[at+1:]
			}
			ref = ref[:at]
		}
		parts := strings.SplitN(ref, "/", 3)
		if len(parts) < 2 {
			return "", "", "", fmt.Errorf("could not parse %q — want hf:Org/Repo[/File.gguf][@rev]", arg)
		}
		repo = parts[0] + "/" + parts[1]
		if len(parts) == 3 {
			file = parts[2]
		}
	}
	if rev == "" {
		rev = "main"
	}
	if strings.Contains(file, "..") || strings.Contains(repo, "..") {
		return "", "", "", fmt.Errorf("bad path in %q", arg)
	}
	return repo, rev, file, nil
}

// Every file in a repo, from the model API's siblings list.
func repoFiles(ctx context.Context, repo string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		hfHost+"/api/models/"+repo, nil)
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("could not reach huggingface.co: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		io.Copy(io.Discard, resp.Body)
		return nil, fmt.Errorf("%s is gated or private — fornax only pulls public repos", repo)
	}
	if resp.StatusCode == http.StatusNotFound {
		io.Copy(io.Discard, resp.Body)
		return nil, fmt.Errorf("no huggingface.co repo %q — check the name, or `fornax search` for it", repo)
	}
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		return nil, fmt.Errorf("huggingface.co answered HTTP %d for %s", resp.StatusCode, repo)
	}
	var meta struct {
		Siblings []struct {
			RFilename string `json:"rfilename"`
		} `json:"siblings"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, openai.MaxBody)).Decode(&meta); err != nil {
		return nil, fmt.Errorf("the repo listing was not readable: %w", err)
	}
	var files []string
	for _, s := range meta.Siblings {
		files = append(files, s.RFilename)
	}
	return files, nil
}

// Quant preference when the repo offers a choice — middle quants first,
// giants and full-precision last. Substring match against the whole path
// so unsloth's `UD-Q4_K_XL/…` directories rank too.
var quantRank = []string{
	"Q4_K_M", "Q5_K_M", "Q6_K", "Q4_K_XL", "Q8_0",
	"Q4_0", "Q3_K_M", "IQ4_XS", "F16", "BF16",
}

// Pick the one .gguf a repo-only pull should get. Projectors and other
// non-weight ggufs are never the answer.
func pickGGUFFile(repo string, files []string) (string, error) {
	var cands []string
	for _, f := range files {
		base := strings.ToLower(filepath.Base(f))
		if !strings.HasSuffix(base, ".gguf") || strings.Contains(base, "mmproj") {
			continue
		}
		cands = append(cands, f)
	}
	if len(cands) == 0 {
		return "", fmt.Errorf("%s has no .gguf weights — check the files at huggingface.co/%s/tree/main", repo, repo)
	}
	for _, quant := range quantRank {
		for _, f := range cands {
			if strings.Contains(strings.ToUpper(f), quant) {
				return f, nil
			}
		}
	}
	if len(cands) == 1 {
		return cands[0], nil
	}
	return "", fmt.Errorf("%s has %d .gguf files fornax cannot rank — pick one: `fornax pull hf:%s/<file>`", repo, len(cands), repo)
}

var splitPartPattern = regexp.MustCompile(`^(.+-)(\d{5})-of-(\d{5})\.gguf$`)

// If the picked file is part 1 of a split archive, the remaining parts in
// the same directory come along as companions (llama-server finds the
// siblings next to part 1).
func splitCompanions(files []string, picked string) []string {
	m := splitPartPattern.FindStringSubmatch(filepath.Base(picked))
	if m == nil || m[2] != "00001" {
		return nil
	}
	dir := filepath.Dir(picked)
	var parts []string
	for _, f := range files {
		if f == picked || filepath.Dir(f) != dir {
			continue
		}
		if pm := splitPartPattern.FindStringSubmatch(filepath.Base(f)); pm != nil && pm[1] == m[1] && pm[3] == m[3] {
			parts = append(parts, f)
		}
	}
	sort.Strings(parts)
	return parts
}

// A projector sibling for vision/audio/speech models — prefer one in the
// picked file's own directory, else any mmproj in the repo.
func findMMProj(files []string, picked string) string {
	var fallback string
	dir := filepath.Dir(picked)
	for _, f := range files {
		if !strings.Contains(strings.ToLower(filepath.Base(f)), "mmproj") {
			continue
		}
		if filepath.Dir(f) == dir {
			return f
		}
		if fallback == "" {
			fallback = f
		}
	}
	return fallback
}

// The modality a repo name implies when --kind is not given. Vision and
// audio only apply when a projector exists — a bare "vl" repo without one
// still chats.
func inferKind(repo, file string, hasMMProj bool) string {
	s := strings.ToLower(repo + "/" + file)
	switch {
	case strings.Contains(s, "rerank"):
		return "rerank"
	case strings.Contains(s, "embed"), strings.Contains(s, "bge-"), strings.Contains(s, "arctic-embed"):
		return "embed"
	case strings.Contains(s, "tts"), strings.Contains(s, "outetts"):
		return "speech"
	case hasMMProj && (strings.Contains(s, "asr") || strings.Contains(s, "whisper") ||
		strings.Contains(s, "ultravox") || strings.Contains(s, "vox") || strings.Contains(s, "audio")):
		return "audio"
	case hasMMProj && (strings.Contains(s, "-vl") || strings.Contains(s, "vision") || strings.Contains(s, "pixtral")):
		return "vision"
	}
	return "text"
}

// HEAD the resolve URL: x-linked-etag is the LFS sha256, x-linked-size the
// bytes, x-repo-commit the immutable revision to pin.
func resolveHFPin(ctx context.Context, repo, rev, file string) (*catalog.Pin, error) {
	url := fmt.Sprintf("%s/%s/resolve/%s/%s", hfHost, repo, rev, file)
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
	return &catalog.Pin{File: file, Revision: commit, Bytes: size, SHA256: sha}, nil
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

// The id a custom model lands under: --as when given, otherwise derived from
// the reference. It has to be clean and still free.
func customID(root, as, derived string) (string, error) {
	id := as
	if id == "" {
		id = derived
	}
	if id != strings.Trim(idClean.ReplaceAllString(strings.ToLower(id), "-"), "-") {
		return "", fmt.Errorf("--as %q is not a clean id — use lowercase letters, digits, dashes", as)
	}
	if model(id) != nil || customSpec(root, id) != nil {
		return "", fmt.Errorf("%q is taken — pick another with --as (or `fornax rm %s` first)", id, id)
	}
	return id, nil
}

// Record an installed custom model on a port nothing else claims.
func saveCustom(root string, store *customStore, entry customEntry) error {
	entry.Port = nextCustomPort(store)
	store.Models = append(store.Models, entry)
	if err := saveCustoms(root, store); err != nil {
		return fmt.Errorf("model installed but could not save %s: %w", customFile, err)
	}
	return nil
}

func nextCustomPort(store *customStore) int {
	used := map[int]bool{}
	for _, spec := range catalog.Models() {
		used[spec.Port] = true
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

// A ref is the same model spelled the way the user typed it — `hf:Org/Repo`,
// `hf:Org/Repo/File.gguf`, a huggingface.co URL — normalized so repeat
// resolves skip the network entirely.
func canonicalHFRef(repo, rev, file string) string {
	ref := "hf:" + repo
	if file != "" {
		ref += "/" + file
	}
	if rev != "" && rev != "main" {
		ref += "@" + rev
	}
	return ref
}

// The entry a ref already saved, if it did — by canonical ref or by the
// repo/file/revision it resolved to.
func findCustom(store *customStore, ref, repo, rev, file string) *customEntry {
	for i := range store.Models {
		m := &store.Models[i]
		if m.Ref == ref {
			return m
		}
		if m.Repo == repo && m.Want == rev && (file == "" || m.File == file) {
			return m
		}
		// Entries saved before refs existed carry no Ref/Want.
		if m.Ref == "" && m.Repo == repo && m.File == file {
			return m
		}
	}
	return nil
}

// Turn a ref into a saved custom model: parse, list the repo when the file
// or projector needs picking, pin every artifact, and save. Nothing is
// downloaded — pull() does that once the caller has a spec.
func ensureHF(ctx context.Context, arg, as, kind, mmproj, revFlag string, with companionFlags, sdArgs []string) (*customEntry, error) {
	repo, revision, file, err := parseHFRef(arg, revFlag)
	if err != nil {
		return nil, err
	}
	root := paths.Home()
	store, err := loadCustoms(root)
	if err != nil {
		return nil, err
	}
	diffusion := kind == "image" || kind == "video"
	switch kind {
	case "", "text", "vision", "audio", "embed", "rerank", "speech", "image", "video":
	default:
		return nil, fmt.Errorf("--kind must be text, vision, audio, image, video, embed, rerank or speech")
	}
	if !diffusion && (len(with) > 0 || len(sdArgs) > 0) {
		return nil, fmt.Errorf("--with and --args belong to --kind image or video")
	}
	if existing := findCustom(store, canonicalHFRef(repo, revision, file), repo, revision, file); existing != nil {
		return existing, nil
	}
	fetching := ui.Spin("resolving " + repo)
	var files []string
	if file == "" || !diffusion {
		if files, err = repoFiles(ctx, repo); err != nil {
			fetching.Stop("")
			return nil, err
		}
	}
	if file == "" {
		if file, err = pickGGUFFile(repo, files); err != nil {
			fetching.Stop("")
			return nil, err
		}
	}
	if kind == "" {
		kind = inferKind(repo, file, findMMProj(files, file) != "")
	}
	needsProj := kind == "vision" || kind == "audio" || kind == "speech"
	if mmproj == "" && needsProj {
		mmproj = findMMProj(files, file)
	}
	if needsProj && mmproj == "" {
		fetching.Stop("")
		return nil, fmt.Errorf("--kind %s needs a projector — %s has no mmproj-*.gguf; pass --mmproj <file>", kind, repo)
	}
	pin, err := resolveHFPin(ctx, repo, revision, file)
	if err != nil {
		fetching.Stop("")
		return nil, err
	}
	derived := deriveHFID(file)
	if as == "" && strings.HasSuffix(strings.ToLower(repo), "-gguf") {
		// A repo-only pull reads better named after the repo: unsloth/
		// DeepSeek-V4-Flash-0731-GGUF → hf-deepseek-v4-flash-0731.
		derived = "hf-" + strings.Trim(idClean.ReplaceAllString(
			strings.ToLower(strings.TrimSuffix(repo[strings.Index(repo, "/")+1:], "-GGUF")), "-"), "-")
	}
	id, err := customID(root, as, derived)
	if err != nil {
		fetching.Stop("")
		return nil, err
	}
	entry := &customEntry{
		ID: id, Kind: kind, Repo: repo, Revision: pin.Revision,
		File: file, Bytes: pin.Bytes, SHA256: pin.SHA256,
		Ref: canonicalHFRef(repo, revision, file), Want: revision,
	}
	if mmproj != "" {
		proj, err := resolveHFPin(ctx, repo, revision, mmproj)
		if err != nil {
			fetching.Stop("")
			return nil, err
		}
		entry.MMProj = &customPin{File: mmproj, Bytes: proj.Bytes, SHA256: proj.SHA256}
	}
	for _, part := range splitCompanions(files, file) {
		partPin, err := resolveHFPin(ctx, repo, revision, part)
		if err != nil {
			fetching.Stop("")
			return nil, err
		}
		entry.Companions = append(entry.Companions, customCompanion{
			File: part, Bytes: partPin.Bytes, SHA256: partPin.SHA256,
			URL: fmt.Sprintf("https://huggingface.co/%s/resolve/%s/%s", repo, partPin.Revision, part),
		})
	}
	for _, companion := range with {
		cRepo, cRev, cFile, err := parseHFRef(companion.ref, "")
		if err != nil {
			fetching.Stop("")
			return nil, err
		}
		cPin, err := resolveHFPin(ctx, cRepo, cRev, cFile)
		if err != nil {
			fetching.Stop("")
			return nil, err
		}
		entry.Companions = append(entry.Companions, customCompanion{
			Flag: companion.flag, File: filepath.Base(cFile), Bytes: cPin.Bytes, SHA256: cPin.SHA256,
			URL: fmt.Sprintf("https://huggingface.co/%s/resolve/%s/%s", cRepo, cPin.Revision, cFile),
		})
	}
	entry.Args = sdArgs
	fetching.Stop("")
	if err := saveCustom(root, store, *entry); err != nil {
		return nil, err
	}
	return entry, nil
}

// `fornax pull hf:Org/Repo[/File.gguf] [--as name] [--kind …]`, or paste a
// huggingface.co URL. No file picks a sensible quant; split archives pull
// every part; a repo mmproj attaches itself when the kind wants one.
func cmdPullHF(ctx context.Context, args []string) error {
	set := flag.NewFlagSet("pull", flag.ExitOnError)
	as := set.String("as", "", "custom id to save the model under")
	kind := set.String("kind", "", "text | vision | audio | image | video | embed | rerank | speech (default: infer)")
	var with companionFlags
	set.Var(&with, "with", "image/video: a file sd-cli loads beside the weights, as <sd-cli flag>=hf:Org/Repo/File (repeatable)")
	sdArgs := set.String("args", "", "image/video: sd-cli arguments saved with the model")
	mmproj := set.String("mmproj", "", "projector file in the same repo (default: auto-detect for vision/audio/speech)")
	rev := set.String("rev", "", "branch, tag or commit (default: main)")
	set.Usage = ui.UsageFunc(set, `usage: fornax pull hf:Org/Repo[/File.gguf] [--as name] [--kind K] [--mmproj F]
       (or paste a huggingface.co repo/blob/resolve URL)`)
	ref := parseFlexible(set, args, 1)
	if len(ref) != 1 {
		return fmt.Errorf("usage: fornax pull hf:Org/Repo[/File.gguf] [--as name] [--kind K]")
	}
	entry, err := ensureHF(ctx, ref[0], *as, *kind, *mmproj, *rev, with, strings.Fields(*sdArgs))
	if err != nil {
		return err
	}
	spec := entry.spec()
	if entry.Kind == "image" || entry.Kind == "video" {
		if _, _, err := prepareSD(ctx, spec); err != nil {
			return err
		}
	} else {
		eng, err := llamaEngine()
		if err != nil {
			return err
		}
		if err := pull(ctx, spec, eng); err != nil {
			return err
		}
	}
	switch entry.Kind {
	case "image":
		fmt.Fprintf(os.Stderr, "%s saved as %s — `fornax imagine %s \"…\"`\n", ui.Green("✓"), ui.Bold(entry.ID), entry.ID)
	case "video":
		fmt.Fprintf(os.Stderr, "%s saved as %s — `fornax animate %s \"…\"`\n", ui.Green("✓"), ui.Bold(entry.ID), entry.ID)
	default:
		fmt.Fprintf(os.Stderr, "%s saved as %s — `fornax ask %s …` / `fornax run %s`\n",
			ui.Green("✓"), ui.Bold(entry.ID), entry.ID, entry.ID)
	}
	return nil
}

type companionFlag struct{ flag, ref string }

type companionFlags []companionFlag

func (c *companionFlags) String() string { return "" }

func (c *companionFlags) Set(value string) error {
	name, ref, ok := strings.Cut(value, "=")
	name = strings.TrimLeft(name, "-")
	if !ok || name == "" || ref == "" {
		return fmt.Errorf("--with wants <sd-cli flag>=hf:Org/Repo/File, got %q", value)
	}
	*c = append(*c, companionFlag{flag: name, ref: ref})
	return nil
}
