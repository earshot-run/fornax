package studio

// The Models page: picks that fit this machine, Hugging Face search, and
// downloads you can watch. A download is `fornax pull … --events` run as a
// child, so the page gets exactly what the CLI does — the same resolve, the
// same save at first fetch — and its progress lines.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/earshot-run/fornax/internal/modelrt"
	"github.com/earshot-run/fornax/internal/paths"
)

// A suggested model. There is no catalog to keep current: a pick is only a
// ref plus the companion files and engine arguments it needs, and pulling
// one saves it at first fetch like any `pull hf:`. Bytes is the download
// size, for the page to judge the fit before anything is fetched.
type hubPick struct {
	Key   string `json:"key"`
	Title string `json:"title"`
	Blurb string `json:"blurb"`
	Kind  string `json:"kind"`
	Ref   string `json:"ref"`
	Bytes int64  `json:"bytes"`

	as, pullKind string
	with         []string
	args         string
}

var hubPicks = []hubPick{
	{Key: "qwen3-4b", Title: "Qwen3 4B", Kind: "chat", Bytes: 2_497_280_256,
		Blurb: "Quick and capable. A good first model.",
		Ref:   "hf:Qwen/Qwen3-4B-GGUF/Qwen3-4B-Q4_K_M.gguf"},
	{Key: "gemma-3-4b", Title: "Gemma 3 4B", Kind: "chat", Bytes: 3_341_008_960,
		Blurb: "Google's small model. It can look at images too.",
		Ref:   "hf:ggml-org/gemma-3-4b-it-GGUF/gemma-3-4b-it-Q4_K_M.gguf", pullKind: "vision"},
	{Key: "qwen2.5-omni-3b", Title: "Qwen2.5 Omni 3B", Kind: "chat", Bytes: 3_642_962_976,
		Blurb: "Sees images and hears audio. Ask it about a photo or a recording.",
		Ref:   "hf:ggml-org/Qwen2.5-Omni-3B-GGUF/Qwen2.5-Omni-3B-Q4_K_M.gguf", pullKind: "audio"},
	{Key: "qwen3-14b", Title: "Qwen3 14B", Kind: "chat", Bytes: 9_001_752_960,
		Blurb: "Noticeably sharper than 4B, if you have the memory.",
		Ref:   "hf:Qwen/Qwen3-14B-GGUF/Qwen3-14B-Q4_K_M.gguf"},
	{Key: "gpt-oss-20b", Title: "gpt-oss 20B", Kind: "chat", Bytes: 12_109_566_624,
		Blurb: "OpenAI's open model. Strong at reasoning and code.",
		Ref:   "hf:ggml-org/gpt-oss-20b-GGUF/gpt-oss-20b-MXFP4.gguf"},
	{Key: "sdxl-lightning", Title: "SDXL Lightning", Kind: "image", Bytes: 4_098_988_672,
		Blurb: "Good pictures in four steps. Fast even without a big GPU.",
		Ref:   "hf:mzwing/SDXL-Lightning-GGUF/sdxl_lightning_4step.q8_0.gguf", pullKind: "image",
		args: "--cfg-scale 1 --sampling-method euler --steps 4"},
	{Key: "qwen-image-2.1", Title: "Qwen-Image 2.1", Kind: "image", Bytes: 14_142_739_960,
		Blurb: "The best quality here, and it edits photos you give it. Slow without a GPU.",
		Ref:   "hf:leejet/Qwen-Image-2.1-GGUF/qwen_image_2.1-Q8_0.gguf", pullKind: "image",
		with: []string{
			"vae=hf:Comfy-Org/Qwen-Image-2.1/vae/qwen_image_2.1_vae_bf16.safetensors",
			"llm=hf:Qwen/Qwen3-VL-8B-Instruct-GGUF/Qwen3VL-8B-Instruct-Q4_K_M.gguf",
			"llm_vision=hf:Qwen/Qwen3-VL-8B-Instruct-GGUF/mmproj-Qwen3VL-8B-Instruct-Q8_0.gguf",
		},
		args: "--cfg-scale 6.0 --sampling-method euler"},
	{Key: "qwen3-tts", Title: "Qwen3 TTS 1.7B", Kind: "speech", Bytes: 2_294_297_312,
		Blurb: "Natural speech in ten languages. Clones a voice from a few seconds of audio.",
		Ref:   "hf:ggml-org/Qwen3-TTS-12Hz-1.7B-Base-GGUF/Qwen3-TTS-12Hz-1.7B-Base-Q8_0.gguf"},
	{Key: "wan-2.2-5b", Title: "Wan 2.2 5B", Kind: "video", Bytes: 8_497_662_272,
		Blurb: "Short clips from a prompt or a still image. Wants a GPU.",
		Ref:   "hf:QuantStack/Wan2.2-TI2V-5B-GGUF/Wan2.2-TI2V-5B-Q4_K_M.gguf", pullKind: "video",
		with: []string{
			"vae=hf:Comfy-Org/Wan_2.2_ComfyUI_Repackaged/split_files/vae/wan2.2_vae.safetensors",
			"t5xxl=hf:city96/umt5-xxl-encoder-gguf/umt5-xxl-encoder-Q4_K_M.gguf",
		},
		args: "--cfg-scale 6.0 --sampling-method euler --flow-shift 3.0 --diffusion-fa"},
}

// The `fornax pull` arguments that fetch a pick.
func (p *hubPick) pullArgs() []string {
	args := []string{"pull", p.Ref, "--events"}
	if p.as != "" {
		args = append(args, "--as", p.as)
	}
	if p.pullKind != "" {
		args = append(args, "--kind", p.pullKind)
	}
	for _, w := range p.with {
		args = append(args, "--with", w)
	}
	if p.args != "" {
		args = append(args, "--args", p.args)
	}
	return args
}

type hubDownload struct {
	ID    string `json:"id"`
	Key   string `json:"key,omitempty"`
	Ref   string `json:"ref"`
	Title string `json:"title"`
	Kind  string `json:"kind,omitempty"`
	// running, done, failed
	State string `json:"state"`
	// The file being fetched right now, and how far along the whole pull is.
	Step  string `json:"step,omitempty"`
	Done  int64  `json:"done"`
	Total int64  `json:"total"`
	Model string `json:"model,omitempty"`
	Error string `json:"error,omitempty"`

	expected int64
	files    map[string][2]int64
	cancel   context.CancelFunc
}

func (s *studio) hubRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/hub/picks", s.handlePicks)
	mux.HandleFunc("GET /api/hub/popular", s.handlePopular)
	mux.HandleFunc("GET /api/hub/search", s.handleSearch)
	mux.HandleFunc("GET /api/hub/preview", s.handlePreview)
	mux.HandleFunc("POST /api/hub/downloads", s.handleDownload)
	mux.HandleFunc("DELETE /api/hub/downloads/{id}", s.handleCancelDownload)
	mux.HandleFunc("DELETE /api/hub/models/{id}", s.handleRemoveModel)
	mux.HandleFunc("GET /api/hub/token", s.handleTokenStatus)
	mux.HandleFunc("PUT /api/hub/token", s.handleSaveToken)
	mux.HandleFunc("DELETE /api/hub/token", s.handleClearToken)
}

// Whether a Hugging Face token is set and whose it is. The token itself
// never goes back to the page.
func (s *studio) handleTokenStatus(w http.ResponseWriter, r *http.Request) {
	token := paths.HFToken()
	status := map[string]any{"set": token != ""}
	if os.Getenv("HF_TOKEN") != "" {
		status["source"] = "HF_TOKEN"
	}
	if token != "" {
		if name, err := modelrt.HFWhoAmI(r.Context(), token); err == nil {
			status["account"] = name
		} else {
			status["error"] = err.Error()
		}
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *studio) handleSaveToken(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<12)).Decode(&body); err != nil || strings.TrimSpace(body.Token) == "" {
		http.Error(w, "paste a token first", http.StatusBadRequest)
		return
	}
	token := strings.TrimSpace(body.Token)
	name, err := modelrt.HFWhoAmI(r.Context(), token)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := setHFToken(s.root, token); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"set": true, "account": name})
}

func (s *studio) handleClearToken(w http.ResponseWriter, _ *http.Request) {
	if err := setHFToken(s.root, ""); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// config.json is written mode 600, like the server key it already holds.
func setHFToken(root, token string) error {
	cfg, err := paths.LoadConfig(root)
	if err != nil {
		return err
	}
	cfg.Version = 1
	cfg.HFToken = token
	return paths.SaveConfig(root, cfg)
}

type pickView struct {
	hubPick
	Fit       string `json:"fit"`
	Installed string `json:"installed,omitempty"`
}

func (s *studio) handlePicks(w http.ResponseWriter, _ *http.Request) {
	memory := modelrt.MemoryBytes()
	installed := map[string]string{}
	for _, spec := range modelrt.AllSpecs(s.root) {
		if spec.Repo != "" && modelrt.Installed(s.root, spec) {
			installed[spec.Repo+"/"+spec.Model.File] = spec.ID
		}
	}
	picks := make([]pickView, 0, len(hubPicks))
	for _, p := range hubPicks {
		repo, file := splitRef(p.Ref)
		picks = append(picks, pickView{hubPick: p, Fit: modelrt.Fit(p.Bytes).String(), Installed: installed[repo+"/"+file]})
	}
	writeJSON(w, http.StatusOK, map[string]any{"memory": memory, "picks": picks})
}

// hf:Org/Repo/path/File → (Org/Repo, path/File).
func splitRef(ref string) (repo, file string) {
	parts := strings.SplitN(strings.TrimPrefix(ref, "hf:"), "/", 3)
	if len(parts) < 3 {
		return strings.Join(parts, "/"), ""
	}
	return parts[0] + "/" + parts[1], parts[2]
}

func (s *studio) handlePopular(w http.ResponseWriter, r *http.Request) {
	kind := r.URL.Query().Get("kind")
	switch kind {
	case "", "chat", "image", "speech", "video":
	default:
		http.Error(w, "unknown kind", http.StatusBadRequest)
		return
	}
	if kind == "chat" {
		kind = ""
	}
	order, ok := hubSort(w, r)
	if !ok {
		return
	}
	hits, err := modelrt.PopularHFWithSort(r.Context(), kind, 20, order)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, http.StatusOK, hits)
}

func (s *studio) handleSearch(w http.ResponseWriter, r *http.Request) {
	order, ok := hubSort(w, r)
	if !ok {
		return
	}
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if query == "" {
		writeJSON(w, http.StatusOK, []modelrt.SearchHit{})
		return
	}
	hits, err := modelrt.SearchHFWithSort(r.Context(), query, 20, order)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, http.StatusOK, hits)
}

// Reject unsupported sorts before making an upstream request.
func hubSort(w http.ResponseWriter, r *http.Request) (string, bool) {
	order := r.URL.Query().Get("sort")
	if order == "" {
		order = "downloads"
	}
	if order != "downloads" && order != "lastModified" {
		http.Error(w, "sort must be downloads or lastModified", http.StatusBadRequest)
		return "", false
	}
	return order, true
}

func (s *studio) handlePreview(w http.ResponseWriter, r *http.Request) {
	ref := r.URL.Query().Get("ref")
	if !strings.HasPrefix(ref, "hf:") || !modelrt.IsHFRef(ref) {
		http.Error(w, "preview takes an hf: ref", http.StatusBadRequest)
		return
	}
	p, err := modelrt.PreviewHF(r.Context(), ref)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"preview": p, "fit": modelrt.Fit(p.Bytes).String()})
}

type downloadRequest struct {
	Key   string `json:"key"`
	Ref   string `json:"ref"`
	Bytes int64  `json:"bytes"`
	Kind  string `json:"kind"`
}

func (s *studio) handleDownload(w http.ResponseWriter, r *http.Request) {
	var req downloadRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, "bad request body", http.StatusBadRequest)
		return
	}
	var pick hubPick
	switch {
	case req.Key != "":
		i := slices.IndexFunc(hubPicks, func(p hubPick) bool { return p.Key == req.Key })
		if i < 0 {
			http.Error(w, "no such pick", http.StatusBadRequest)
			return
		}
		pick = hubPicks[i]
	// Only explicit refs: the child pull also takes bare Org/Repo and ids,
	// which a page has no business sending.
	case strings.HasPrefix(req.Ref, "hf:") && modelrt.IsHFRef(req.Ref), strings.HasPrefix(req.Ref, "ollama:") && modelrt.IsOllamaRef(req.Ref):
		_, file := splitRef(req.Ref)
		pick = hubPick{Ref: req.Ref, Title: strings.TrimPrefix(req.Ref, "hf:"), Bytes: req.Bytes}
		if file == "" {
			pick.Title = strings.TrimSuffix(pick.Title, "/")
		}
		switch req.Kind {
		case "", "image", "video", "speech":
			pick.pullKind = req.Kind
		default:
			http.Error(w, "unknown kind", http.StatusBadRequest)
			return
		}
	default:
		http.Error(w, "download takes a pick or an hf:/ollama: ref", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	for _, d := range s.downloads {
		if d.Ref == pick.Ref && d.State == "running" {
			s.mu.Unlock()
			writeJSON(w, http.StatusOK, d)
			return
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	d := &hubDownload{ID: newStudioID(), Key: pick.Key, Ref: pick.Ref, Title: pick.Title, Kind: pick.pullKind, State: "running",
		expected: pick.Bytes, Total: pick.Bytes, files: map[string][2]int64{}, cancel: cancel}
	s.downloads = append(s.downloads, d)
	s.notifyLocked()
	view := *d
	s.mu.Unlock()
	go s.runDownload(ctx, d, pick.pullArgs())
	writeJSON(w, http.StatusOK, view)
}

// Cancels a running download, or clears a finished one from the list.
func (s *studio) handleCancelDownload(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, d := range s.downloads {
		if d.ID != id {
			continue
		}
		if d.State == "running" {
			d.cancel()
		} else {
			s.downloads = slices.Delete(s.downloads, i, i+1)
		}
		s.notifyLocked()
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.NotFound(w, r)
}

// The child is fornax itself, so it runs with the caller's environment
// (proxies, FORNAX_HOME); it scrubs the environment of every engine it
// starts, as always.
func (s *studio) fornaxCommand(ctx context.Context, args ...string) (*exec.Cmd, error) {
	exe := s.fornaxPath
	if exe == "" {
		var err error
		if exe, err = os.Executable(); err != nil {
			return nil, err
		}
	}
	return exec.CommandContext(ctx, exe, args...), nil
}

func (s *studio) runDownload(ctx context.Context, d *hubDownload, args []string) {
	err := s.pullEvents(ctx, d, args)
	canceled := ctx.Err() != nil
	s.mu.Lock()
	defer s.mu.Unlock()
	d.cancel()
	switch {
	case canceled:
		s.downloads = slices.DeleteFunc(s.downloads, func(x *hubDownload) bool { return x == d })
	case err != nil:
		d.State, d.Error = "failed", err.Error()
	default:
		d.State, d.Done = "done", d.Total
	}
	s.notifyLocked()
}

func (s *studio) pullEvents(ctx context.Context, d *hubDownload, args []string) error {
	cmd, err := s.fornaxCommand(ctx, args...)
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	tail := &lineTail{max: 4}
	stderr := &tailWriter{tail: tail}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	var failure string
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		var ev struct {
			Event   string `json:"event"`
			Label   string `json:"label"`
			Done    int64  `json:"done"`
			Total   int64  `json:"total"`
			Model   string `json:"model"`
			Message string `json:"message"`
		}
		if json.Unmarshal(scanner.Bytes(), &ev) != nil {
			continue
		}
		switch ev.Event {
		case "progress":
			s.downloadProgress(d, ev.Label, ev.Done, ev.Total)
		case "installed":
			s.mu.Lock()
			d.Model = ev.Model
			s.mu.Unlock()
		case "error":
			failure = ev.Message
		}
	}
	err = cmd.Wait()
	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case failure != "":
		return errors.New(failure)
	case err != nil:
		return fmt.Errorf("fornax pull exited (%v): %s", err, tail.String())
	}
	return nil
}

// Progress events arrive per file; the page shows the whole pull. The
// total is the pick's known size, or what the files announced so far.
func (s *studio) downloadProgress(d *hubDownload, label string, done, total int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d.files[label] = [2]int64{done, total}
	var sumDone, sumTotal int64
	for _, f := range d.files {
		sumDone += f[0]
		sumTotal += f[1]
	}
	d.Step, d.Done, d.Total = label, sumDone, max(d.expected, sumTotal)
	if now := time.Now(); now.Sub(s.lastProgress) > 250*time.Millisecond || done >= total {
		s.lastProgress = now
		s.notifyLocked()
	}
}

func (s *studio) handleRemoveModel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	spec := modelrt.Model(id)
	if spec == nil {
		http.NotFound(w, r)
		return
	}
	// A loaded model holds its files open; let it go first.
	s.chat.unload(id)
	s.sd.unload(id)
	cmd, err := s.fornaxCommand(r.Context(), "rm", id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		http.Error(w, strings.TrimSpace(string(out)), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type tailWriter struct {
	mu   sync.Mutex
	tail *lineTail
	buf  []byte
}

func (t *tailWriter) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	for {
		i := strings.IndexAny(string(t.buf), "\r\n")
		if i < 0 {
			break
		}
		t.tail.add(string(t.buf[:i]))
		t.buf = t.buf[i+1:]
	}
	return len(p), nil
}

// The --steps a model's saved engine arguments set, or 0: a pick like SDXL
// Lightning only works at its own step count, and the page defaults to it.
func savedSteps(args []string) int {
	for i, a := range args {
		if a == "--steps" && i+1 < len(args) {
			n, _ := strconv.Atoi(args[i+1])
			return n
		}
	}
	return 0
}
