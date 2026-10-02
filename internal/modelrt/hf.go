package modelrt

// User-added models from Hugging Face. `fornax pull hf:Org/Repo/File.gguf`
// (or a pasted huggingface.co URL) resolves at fetch time — HF serves each
// file's byte count in `x-linked-size` — saves it to ~/.fornax/custom.json,
// and installs through the same path as the built-ins.

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/earshot-run/fornax/internal/catalog"
	"github.com/earshot-run/fornax/internal/openai"
	"github.com/earshot-run/fornax/internal/paths"
	"github.com/earshot-run/fornax/internal/ui"
)

// Where saved custom models live, under the fornax home.
const CustomFile = "custom.json"

// Overridable in tests — the real value hits huggingface.co.
var HFHost = "https://huggingface.co"

// A saved custom model. Serialized — catalog.Spec isn't.
type customEntry struct {
	ID   string `json:"id"`
	Kind string `json:"kind"` // text | vision | audio | image | video | embed | rerank | speech
	// The reference it was pulled as — `hf:Org/Repo` resolves to the same
	// entry without touching the network again.
	Ref  string `json:"ref,omitempty"`
	Repo string `json:"repo"`
	// The ref's requested rev ("main" when unspecified), and the revision
	// its files download from — the same, bar entries saved when fornax
	// still resolved a commit.
	Want     string `json:"want,omitempty"`
	Revision string `json:"revision"`
	File     string `json:"file"`
	Bytes    int64  `json:"bytes"`
	// Direct download URL for non-Hugging-Face sources (ollama registry
	// blobs). Empty = built from repo/revision/file on huggingface.co.
	URL    string      `json:"url,omitempty"`
	SHA256 string      `json:"sha256,omitempty"`
	MMProj *customFile `json:"mmproj,omitempty"`
	// image and video only: files passed to sd-cli under their own flag,
	// and the engine arguments the operator saved for this model.
	Companions []customCompanion `json:"companions,omitempty"`
	Args       []string          `json:"args,omitempty"`
	Port       int               `json:"port"`
}

type customFile struct {
	File   string `json:"file"`
	Bytes  int64  `json:"bytes"`
	URL    string `json:"url,omitempty"`
	SHA256 string `json:"sha256,omitempty"`
}

type customCompanion struct {
	Flag   string `json:"flag"`
	File   string `json:"file"`
	Bytes  int64  `json:"bytes"`
	URL    string `json:"url"`
	SHA256 string `json:"sha256,omitempty"`
}

type customStore struct {
	Version int           `json:"version"`
	Models  []customEntry `json:"models"`
}

func loadCustoms(root string) (*customStore, error) {
	data, err := os.ReadFile(filepath.Join(root, CustomFile))
	if err != nil {
		if os.IsNotExist(err) {
			return &customStore{Version: 1}, nil
		}
		return nil, err
	}
	var store customStore
	if err := json.Unmarshal(data, &store); err != nil {
		return nil, fmt.Errorf("%s is unreadable — delete it to reset custom models", filepath.Join(root, CustomFile))
	}
	return &store, nil
}

func saveCustoms(root string, store *customStore) error {
	store.Version = 1
	return paths.AtomicJSON(filepath.Join(root, CustomFile), store)
}

// The built-ins plus every saved custom model, as specs.
func AllSpecs(root string) []*catalog.Spec {
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
		Model: catalog.Artifact{
			File:     e.File,
			Revision: e.Revision,
			Bytes:    e.Bytes,
			URL:      e.URL,
			SHA256:   e.SHA256,
		},
		Port: e.Port,
	}
	spec.Summary = fmt.Sprintf("custom — %s @ %.7s", e.Repo+"/"+e.File, e.Revision)
	if e.MMProj != nil {
		spec.MMProj = &catalog.Artifact{File: e.MMProj.File, Revision: e.Revision, Bytes: e.MMProj.Bytes, URL: e.MMProj.URL, SHA256: e.MMProj.SHA256}
	}
	if kind == catalog.Image || kind == catalog.Video {
		spec.Runtime = catalog.SD
	}
	// Saved engine arguments: sd-cli flags for image and video models,
	// llama-server flags for everything else.
	spec.Args = e.Args
	for _, c := range e.Companions {
		spec.Companions = append(spec.Companions, catalog.Artifact{Flag: c.Flag, File: c.File, Bytes: c.Bytes, URL: c.URL, SHA256: c.SHA256})
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

// Whether id is a saved custom model rather than a built-in.
func IsCustom(root, id string) bool {
	return customSpec(root, id) != nil
}

// Forget a saved custom model; its files are the caller's to remove.
func DropCustom(root, id string) error {
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
	return repoFilesRevision(ctx, repo, "main")
}

func repoFilesRevision(ctx context.Context, repo, revision string) ([]string, error) {
	endpoint := HFHost + "/api/models/" + repo
	if revision != "main" {
		endpoint += "/revision/" + url.PathEscape(revision)
	}
	req, err := hfRequest(ctx, http.MethodGet, endpoint)
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
		return nil, gatedError(repo)
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
	ranked, err := rankedGGUFFiles(repo, files)
	if err != nil {
		return "", err
	}
	return ranked[0], nil
}

// Every .gguf a repo-only pull could get, best quant first. Projectors and
// other non-weight ggufs are never included.
func rankedGGUFFiles(repo string, files []string) ([]string, error) {
	var cands []string
	for _, f := range files {
		base := strings.ToLower(filepath.Base(f))
		if !strings.HasSuffix(base, ".gguf") || strings.Contains(base, "mmproj") {
			continue
		}
		if part := splitPartPattern.FindStringSubmatch(filepath.Base(f)); part != nil && part[2] != "00001" {
			continue
		}
		cands = append(cands, f)
	}
	if len(cands) == 0 {
		return nil, fmt.Errorf("%s has no .gguf weights — check the files at huggingface.co/%s/tree/main", repo, repo)
	}
	var ranked []string
	for _, quant := range quantRank {
		for _, f := range cands {
			if strings.Contains(strings.ToUpper(f), quant) && !slices.Contains(ranked, f) {
				ranked = append(ranked, f)
			}
		}
	}
	if len(ranked) == 0 {
		if len(cands) == 1 {
			return cands, nil
		}
		return nil, fmt.Errorf("%s has %d .gguf files fornax cannot rank — pick one: `fornax pull hf:%s/<file>`", repo, len(cands), repo)
	}
	return ranked, nil
}

// The best-ranked quant this machine can actually run: when a repo offers
// several, the first whose size fits total RAM, else the best-ranked one.
// Costs a HEAD per candidate, so only a repo-only pull uses it.
func pickGGUFFileFitting(ctx context.Context, repo, revision string, files []string) (string, error) {
	ranked, err := rankedGGUFFiles(repo, files)
	if err != nil {
		return "", err
	}
	ram := MemoryBytes()
	if len(ranked) == 1 || ram == 0 {
		return ranked[0], nil
	}
	for _, f := range ranked {
		fetch := append([]string{f}, splitCompanions(files, f)...)
		if projector := findMMProj(files, f); projector != "" {
			kind := inferKind(repo, f, true)
			if kind == "vision" || kind == "audio" || kind == "speech" {
				fetch = append(fetch, projector)
			}
		}
		var size int64
		fits := true
		for _, file := range fetch {
			n, _, err := hfFileSize(ctx, repo, revision, file)
			if err != nil || n > ram || size > ram-n {
				fits = false
				break
			}
			size += n
		}
		if fits && catalog.FitFor(size, ram) != catalog.Wont {
			return f, nil
		}
	}
	return ranked[0], nil
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

// Validate that part 1 can actually bring the whole archive along.
func validateSplitFiles(files []string, picked string) error {
	m := splitPartPattern.FindStringSubmatch(filepath.Base(picked))
	if m == nil {
		return nil
	}
	if m[2] != "00001" {
		return fmt.Errorf("split weights must start with part 1: %s", picked)
	}
	total, _ := strconv.Atoi(m[3])
	if total < 1 {
		return fmt.Errorf("invalid split archive: %s", picked)
	}
	for i := 1; i <= total; i++ {
		part := filepath.Join(filepath.Dir(picked), fmt.Sprintf("%s%05d-of-%s.gguf", m[1], i, m[3]))
		if !slices.Contains(files, part) {
			return fmt.Errorf("split archive is incomplete: missing %s", part)
		}
	}
	return nil
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

// HEAD the resolve URL for a file's size and, when the source publishes one,
// its sha256: x-linked-size and x-linked-etag for LFS files (the etag is the
// LFS object's sha256), the body's own length otherwise.
func hfFileSize(ctx context.Context, repo, rev, file string) (int64, string, error) {
	url := fmt.Sprintf("%s/%s/resolve/%s/%s", HFHost, repo, rev, file)
	req, err := hfRequest(ctx, http.MethodHead, url)
	if err != nil {
		return 0, "", err
	}
	// x-linked-size and x-linked-etag live on huggingface.co's own response —
	// following the CDN redirect would drop them.
	client := &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", fmt.Errorf("could not reach huggingface.co: %w", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return 0, "", gatedError(repo)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		return 0, "", fmt.Errorf("huggingface.co answered HTTP %d for %s", resp.StatusCode, url)
	}
	sha := lfsDigest(resp.Header.Get("x-linked-etag"))
	if size, err := strconv.ParseInt(resp.Header.Get("x-linked-size"), 10, 64); err == nil && size > 0 {
		return size, sha, nil
	}
	if resp.StatusCode < 300 && resp.ContentLength > 0 {
		return resp.ContentLength, sha, nil
	}
	return 0, "", fmt.Errorf("huggingface.co did not say how large %s is", file)
}

// The sha256 in an x-linked-etag, or "" when it is not one (a small
// non-LFS file answers with a plain etag, and some repos hash differently).
func lfsDigest(etag string) string {
	etag = strings.Trim(strings.TrimSpace(etag), `"`)
	if len(etag) != 64 {
		return ""
	}
	if _, err := hex.DecodeString(etag); err != nil {
		return ""
	}
	return strings.ToLower(etag)
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
	if Model(id) != nil || customSpec(root, id) != nil {
		return "", fmt.Errorf("%q is taken — pick another with --as (or `fornax rm %s` first)", id, id)
	}
	return id, nil
}

// Record an installed custom model on a port nothing else claims.
func saveCustom(root string, store *customStore, entry customEntry) error {
	port, err := nextCustomPort(store)
	if err != nil {
		return err
	}
	entry.Port = port
	store.Models = append(store.Models, entry)
	if err := saveCustoms(root, store); err != nil {
		return fmt.Errorf("model installed but could not save %s: %w", CustomFile, err)
	}
	return nil
}

// The first free custom port, or an error when the block is full — a model
// with port 0 would bind a random port and never be findable again.
func nextCustomPort(store *customStore) (int, error) {
	used := map[int]bool{}
	for _, spec := range catalog.Models() {
		used[spec.Port] = true
	}
	for _, m := range store.Models {
		used[m.Port] = true
	}
	for p := 7401; p < scratchPortBase; p++ {
		if !used[p] {
			return p, nil
		}
	}
	return 0, fmt.Errorf("all custom model ports (%d–%d) are taken — `fornax rm` one first", 7401, scratchPortBase-1)
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

// A file sd-cli loads beside an image or video model's weights, under its
// own sd-cli flag: Flag "vae", Ref "hf:Org/Repo/ae.safetensors".
type Companion struct{ Flag, Ref string }

// What `pull hf:…` can say beyond the ref itself. With belongs to image and
// video models; Args is saved engine arguments for any model — sd-cli flags
// for image and video, llama-server flags otherwise.
type HFOptions struct {
	As, Kind, MMProj, Rev string
	With                  []Companion
	Args                  []string
}

// Save an hf: ref (or pasted huggingface.co URL) as a custom
// model — or return the one a previous pull saved. Nothing is downloaded.
func EnsureHF(ctx context.Context, ref string, opts HFOptions) (*catalog.Spec, error) {
	entry, err := ensureHF(ctx, ref, opts.As, opts.Kind, opts.MMProj, opts.Rev, opts.With, opts.Args)
	if err != nil {
		return nil, err
	}
	return entry.spec(), nil
}

// Turn a ref into a saved custom model: parse, list the repo when the file
// or projector needs picking, size every file, and save. Nothing is
// downloaded — Pull() does that once the caller has a spec.
func ensureHF(ctx context.Context, arg, as, kind, mmproj, revFlag string, with []Companion, sdArgs []string) (*customEntry, error) {
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
	if !diffusion && len(with) > 0 {
		return nil, fmt.Errorf("--with names files sd-cli loads, so it belongs to --kind image or video; --args works for any model")
	}
	if existing := findCustom(store, canonicalHFRef(repo, revision, file), repo, revision, file); existing != nil {
		return existing, nil
	}
	fetching := ui.Spin("resolving " + repo)
	var files []string
	if file == "" || !diffusion || splitPartPattern.MatchString(filepath.Base(file)) {
		if files, err = repoFilesRevision(ctx, repo, revision); err != nil {
			fetching.Stop("")
			return nil, err
		}
	}
	if file == "" {
		if file, err = pickGGUFFileFitting(ctx, repo, revision, files); err != nil {
			fetching.Stop("")
			return nil, err
		}
	}
	if err := validateSplitFiles(files, file); err != nil {
		fetching.Stop("")
		return nil, err
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
	size, sha, err := hfFileSize(ctx, repo, revision, file)
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
		ID: id, Kind: kind, Repo: repo, Revision: revision,
		File: file, Bytes: size, SHA256: sha,
		Ref: canonicalHFRef(repo, revision, file), Want: revision,
	}
	if mmproj != "" {
		projSize, projSHA, err := hfFileSize(ctx, repo, revision, mmproj)
		if err != nil {
			fetching.Stop("")
			return nil, err
		}
		entry.MMProj = &customFile{File: mmproj, Bytes: projSize, SHA256: projSHA}
	}
	for _, part := range splitCompanions(files, file) {
		partSize, partSHA, err := hfFileSize(ctx, repo, revision, part)
		if err != nil {
			fetching.Stop("")
			return nil, err
		}
		entry.Companions = append(entry.Companions, customCompanion{
			File: part, Bytes: partSize, SHA256: partSHA,
			URL: fmt.Sprintf("https://huggingface.co/%s/resolve/%s/%s", repo, revision, part),
		})
	}
	for _, companion := range with {
		cRepo, cRev, cFile, err := parseHFRef(companion.Ref, "")
		if err != nil {
			fetching.Stop("")
			return nil, err
		}
		cSize, cSHA, err := hfFileSize(ctx, cRepo, cRev, cFile)
		if err != nil {
			fetching.Stop("")
			return nil, err
		}
		entry.Companions = append(entry.Companions, customCompanion{
			Flag: companion.Flag, File: filepath.Base(cFile), Bytes: cSize, SHA256: cSHA,
			URL: fmt.Sprintf("https://huggingface.co/%s/resolve/%s/%s", cRepo, cRev, cFile),
		})
	}
	entry.Args = sdArgs
	fetching.Stop("")
	if err := saveCustom(root, store, *entry); err != nil {
		return nil, err
	}
	return entry, nil
}
