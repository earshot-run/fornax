// Package studio is `fornax studio` — a browser front end for every local
// model: chat with text, vision and audio models, and a queue that makes
// images, video and speech. It binds loopback only, behind the same
// generated key the model servers use (a link carries it once, then an
// HttpOnly cookie does).
//
// Generations queue and run one at a time — two would fight over the same
// GPU memory — and every finished file lands in studio/library beside a
// JSON sidecar that remembers how it was made. Each kind of generation
// lives in its own file (studio_sd.go, studio_voice.go); chat, which is a
// conversation and not a queue, lives in studio_chat.go.
package studio

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/earshot-run/fornax/internal/catalog"
	"github.com/earshot-run/fornax/internal/modelrt"
	"github.com/earshot-run/fornax/internal/paths"
	"github.com/earshot-run/fornax/internal/ui"
)

//go:embed web
var studioAssets embed.FS

// Just below the built-in model ports (7341+), so a studio never takes one.
const Port = 7340

const (
	studioCookie   = "fornax_studio"
	studioMaxRefs  = 10
	studioMaxBytes = 25 << 20
)

// How a library item was made, saved beside it and shown in the page.
// Fields a kind does not use stay zero and drop out of the JSON.
type studioItem struct {
	ID       string    `json:"id"`
	Kind     string    `json:"kind"`
	Ext      string    `json:"ext"`
	Model    string    `json:"model"`
	Prompt   string    `json:"prompt"`
	Negative string    `json:"negative,omitempty"`
	Width    int       `json:"width,omitempty"`
	Height   int       `json:"height,omitempty"`
	Steps    int       `json:"steps,omitempty"`
	Frames   int       `json:"frames,omitempty"`
	Seed     int64     `json:"seed,omitempty"`
	Refs     []string  `json:"refs,omitempty"`
	Voice    string    `json:"voice,omitempty"`
	Lang     string    `json:"lang,omitempty"`
	Created  time.Time `json:"created"`
	// Measured, so the page can estimate the next run on this machine.
	Seconds       float64 `json:"seconds,omitempty"`
	StepSeconds   float64 `json:"stepSeconds,omitempty"`
	DecodeSeconds float64 `json:"decodeSeconds,omitempty"`
}

type studioJob struct {
	studioItem
	State   string    `json:"state"`
	Phase   string    `json:"phase,omitempty"`
	Step    int       `json:"step"`
	Total   int       `json:"total"`
	Preview int       `json:"preview,omitempty"`
	Started time.Time `json:"started,omitzero"`
	Error   string    `json:"error,omitempty"`

	cancel       context.CancelFunc
	sampled      time.Time
	previewStamp time.Time
}

const (
	jobQueued   = "queued"
	jobRunning  = "running"
	jobFailed   = "failed"
	jobCanceled = "canceled"
)

// What the queue can make. The model a request names must be installed and
// of modelKind; validate fills defaults and rejects what the engine can't
// run; generate writes s.outputPath(job) or fails.
type studioKind struct {
	modelKind catalog.Modality
	ext       string
	validate  func(req *studioRequest) error
	generate  func(ctx context.Context, job *studioJob) error
}

type studio struct {
	root  string
	dir   string
	key   string
	kinds map[string]studioKind
	// One generation, start to finish. Dispatches on the job's kind; tests
	// swap it out.
	generate func(ctx context.Context, job *studioJob) error

	mu      sync.Mutex
	jobs    []*studioJob
	library int
	changed chan struct{}
	wake    chan struct{}

	chat *chatSlot
	sd   *sdSlot
	// Driven over an ssh tunnel (-leash), so the page names this machine.
	remote bool

	downloads    []*hubDownload
	lastProgress time.Time
	lastJournal  time.Time
	shuttingDown bool
	// The fornax binary downloads and removals run as; "" is this process's
	// own. Tests point it at a fake.
	fornaxPath string
}

func newStudio(root, key string) (*studio, error) {
	dir := filepath.Join(root, "studio")
	for _, sub := range []string{"library", "refs", "previews", "chats"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o700); err != nil {
			return nil, err
		}
	}
	s := &studio{root: root, dir: dir, key: key, changed: make(chan struct{}), wake: make(chan struct{}, 1)}
	s.kinds = map[string]studioKind{
		"image":  s.sdKind(catalog.Image, "png"),
		"video":  s.sdKind(catalog.Video, "webm"),
		"speech": s.speechKind(),
	}
	s.generate = func(ctx context.Context, job *studioJob) error {
		return s.kinds[job.Kind].generate(ctx, job)
	}
	if err := s.loadDownloads(); err != nil {
		return nil, err
	}
	s.sd = newSDSlot(dir)
	s.chat = newChatSlot()
	// One model on the GPU at a time: a chat model displaces an idle image one.
	s.chat.beforeLoad = func() { s.sd.unload("") }
	return s, nil
}

// Serve the studio on loopback port until ctx ends. leash is the far end of
// `-on`: it also ends when stdin closes, and exits with a distinct code when
// the port belongs to something else.
func Serve(ctx context.Context, port int, noOpen, leash bool) error {
	if leash {
		ctx = leashed(ctx, os.Stdin)
	}
	root := paths.Home()
	key, err := paths.EnsureKey(root)
	if err != nil {
		return err
	}
	link := fmt.Sprintf("http://127.0.0.1:%d/?key=%s", port, key)
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		if studioAlive(port) {
			fmt.Fprintf(os.Stderr, "%s\n", ui.Dim(fmt.Sprintf("studio is already open on port %d", port)))
			fmt.Println(link)
			if !noOpen {
				openBrowser(link)
			}
			if leash {
				// The tunnel is up only while this session is.
				<-ctx.Done()
			}
			return nil
		}
		if leash {
			fmt.Fprintf(os.Stderr, "port %d is in use by something else\n", port)
			os.Exit(studioLeashBusy)
		}
		return fmt.Errorf("port %d is in use by something else — pick another with -port", port)
	}
	s, err := newStudio(root, key)
	if err != nil {
		listener.Close()
		return err
	}
	s.remote = leash
	srv := &http.Server{
		Handler:           s.handler(port),
		ReadHeaderTimeout: 10 * time.Second,
		// Open streams end with the process, not after Shutdown's grace.
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	go s.work(ctx)
	go s.chat.reap(ctx)
	go s.reapSD(ctx)
	go func() {
		<-ctx.Done()
		s.interruptDownloads()
		shutdown, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	fmt.Fprintf(os.Stderr, "%s studio on %s — Ctrl-C stops\n", ui.Green("✓"), ui.Bold(fmt.Sprintf("http://127.0.0.1:%d", port)))
	fmt.Println(link)
	if !noOpen {
		openBrowser(link)
	}
	err = srv.Serve(listener)
	s.chat.stop()
	s.sd.stop()
	if !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func studioAlive(port int) bool {
	client := http.Client{Timeout: time.Second}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/healthz", port))
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64))
	return string(body) == "fornax studio"
}

// The opener runs with the caller's environment: it needs the desktop
// session (DISPLAY, the launch services bootstrap), and it runs nothing of
// ours.
func openBrowser(link string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", link)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", link)
	default:
		cmd = exec.Command("xdg-open", link)
	}
	if err := cmd.Start(); err == nil {
		go cmd.Wait()
	}
}

func (s *studio) handler(port int) http.Handler {
	mux := http.NewServeMux()
	assets, _ := fs.Sub(studioAssets, "web")
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		http.ServeFileFS(w, r, assets, "index.html")
	})
	mux.HandleFunc("GET /assets/{name}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		http.ServeFileFS(w, r, assets, r.PathValue("name"))
	})
	mux.HandleFunc("GET /api/about", s.handleAbout)
	mux.HandleFunc("GET /api/models", s.handleModels)
	mux.HandleFunc("GET /api/library", s.handleLibrary)
	mux.HandleFunc("DELETE /api/library", s.handleClearLibrary)
	mux.HandleFunc("DELETE /api/library/{id}", s.handleDeleteItem)
	mux.HandleFunc("POST /api/jobs", s.handleCreateJob)
	mux.HandleFunc("DELETE /api/jobs/{id}", s.handleCancelJob)
	mux.HandleFunc("POST /api/refs", s.handleUploadRef)
	mux.HandleFunc("GET /api/events", s.handleEvents)
	mux.HandleFunc("GET /files/{dir}/{name}", s.handleFile)
	s.chatRoutes(mux)
	s.hubRoutes(mux)
	return s.guard(port, mux)
}

// Loopback is not enough on its own: a web page can resolve its own domain
// to 127.0.0.1 (DNS rebinding) or post across origins. The Host check stops
// the first, the key and a SameSite=Strict cookie the second.
func (s *studio) guard(port int, next http.Handler) http.Handler {
	hosts := []string{fmt.Sprintf("127.0.0.1:%d", port), fmt.Sprintf("localhost:%d", port)}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !slices.Contains(hosts, r.Host) {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		if r.URL.Path == "/healthz" {
			io.WriteString(w, "fornax studio")
			return
		}
		if given := r.URL.Query().Get("key"); given != "" && r.Method == http.MethodGet {
			if !s.keyMatches(given) {
				s.denied(w, r)
				return
			}
			http.SetCookie(w, &http.Cookie{
				Name: studioCookie, Value: s.key, Path: "/", MaxAge: 400 * 24 * 3600,
				HttpOnly: true, SameSite: http.SameSiteStrictMode,
			})
			clean := *r.URL
			clean.RawQuery = ""
			http.Redirect(w, r, clean.String(), http.StatusSeeOther)
			return
		}
		cookie, err := r.Cookie(studioCookie)
		if err != nil || !s.keyMatches(cookie.Value) {
			s.denied(w, r)
			return
		}
		if origin := r.Header.Get("Origin"); r.Method != http.MethodGet && origin != "" && !slices.Contains(hosts, strings.TrimPrefix(origin, "http://")) {
			http.Error(w, "cross-origin request refused", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *studio) keyMatches(given string) bool {
	return subtle.ConstantTimeCompare([]byte(given), []byte(s.key)) == 1
}

func (s *studio) denied(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/files/") || strings.HasPrefix(r.URL.Path, "/assets/") {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusUnauthorized)
	io.WriteString(w, `<!doctype html><meta charset="utf-8"><title>fornax studio</title>
<body style="font:15px/1.5 system-ui;display:grid;place-items:center;height:100vh;margin:0;background:#141312;color:#e9e6e1">
<p>Open the link <code>fornax studio</code> printed, or run it again.</p>`)
}

// Which machine the page is driving.
type studioAbout struct {
	Host string `json:"host"`
	OS   string `json:"os"`
	Arch string `json:"arch"`
	// The accelerator chat and image engines run on here (cpu, cuda,
	// vulkan, metal); empty when this platform has no such engine.
	Backend   string `json:"backend"`
	SDBackend string `json:"sdBackend"`
	Remote    bool   `json:"remote"`
}

func (s *studio) handleAbout(w http.ResponseWriter, _ *http.Request) {
	host, _ := os.Hostname()
	about := studioAbout{Host: host, OS: runtime.GOOS, Arch: runtime.GOARCH, Remote: s.remote}
	if eng, err := modelrt.LlamaEngine(); err == nil {
		about.Backend = string(eng.Backend)
	}
	if eng, err := modelrt.SDEngine(); err == nil {
		about.SDBackend = string(eng.Backend)
	}
	writeJSON(w, http.StatusOK, about)
}

type studioModel struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Repo    string `json:"repo,omitempty"`
	File    string `json:"file,omitempty"`
	Kind    string `json:"kind"`
	Runtime string `json:"runtime"`
	Params  string `json:"params,omitempty"`
	Quant   string `json:"quant,omitempty"`
	// Holds a conversation (text, vision, audio); the rest go through the queue.
	Chat  bool   `json:"chat"`
	Bytes int64  `json:"bytes"`
	Fit   string `json:"fit"`
	// The sample steps its saved engine arguments pin, or 0.
	Steps int `json:"steps,omitempty"`
}

// What the page calls a model. Built-ins carry a name; a model pulled by ref
// has only its derived id (hf-qwen3-tts-12hz-1-7b-base), so it shows its
// repo's own name (Qwen3-TTS-12Hz-1.7B-Base). An id chosen with --as stays.
func modelLabel(spec *catalog.Spec) string {
	if spec.Name != "" && spec.Name != spec.ID {
		return spec.Name
	}
	derived := strings.HasPrefix(spec.ID, "hf-") || strings.HasPrefix(spec.ID, "ollama-")
	if !derived || spec.Repo == "" {
		return spec.ID
	}
	name := path.Base(spec.Repo)
	for _, suffix := range []string{"-GGUF", "-gguf", "_GGUF", "-Gguf"} {
		name = strings.TrimSuffix(name, suffix)
	}
	return name
}

// Every installed model, whatever it does; each page picks its own kind.
func (s *studio) handleModels(w http.ResponseWriter, _ *http.Request) {
	models := []studioModel{}
	for _, spec := range modelrt.AllSpecs(s.root) {
		if !modelrt.Installed(s.root, spec) {
			continue
		}
		models = append(models, studioModel{
			ID: spec.ID, Name: modelLabel(spec), Repo: spec.Repo, File: spec.Model.File, Kind: spec.Kind.String(), Runtime: spec.Runtime.String(),
			Params: spec.Params, Quant: spec.Quant(),
			Bytes: spec.TotalBytes(), Fit: modelrt.Fit(spec.TotalBytes()).String(), Steps: savedSteps(spec.Args),
			Chat: modelrt.RequireChat(spec) == nil && spec.Kind != catalog.Decision,
		})
	}
	writeJSON(w, http.StatusOK, models)
}

func (s *studio) handleLibrary(w http.ResponseWriter, r *http.Request) {
	items, err := s.items(r.URL.Query().Get("kind"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, items)
}

// Newest first, of one kind ("" for all). A file without its sidecar is a
// generation still being written, or one that died; neither belongs here.
func (s *studio) items(kind string) ([]studioItem, error) {
	entries, err := os.ReadDir(filepath.Join(s.dir, "library"))
	if err != nil {
		return nil, err
	}
	items := []studioItem{}
	for _, entry := range entries {
		id, ok := strings.CutSuffix(entry.Name(), ".json")
		if !ok || !studioIDPattern.MatchString(id) {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(s.dir, "library", entry.Name()))
		if err != nil {
			continue
		}
		var item studioItem
		if json.Unmarshal(raw, &item) != nil || (kind != "" && item.Kind != kind) {
			continue
		}
		if _, err := os.Stat(s.outputPath(&item)); err != nil {
			continue
		}
		items = append(items, item)
	}
	slices.SortFunc(items, func(a, b studioItem) int { return b.Created.Compare(a.Created) })
	return items, nil
}

func (s *studio) handleDeleteItem(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !studioIDPattern.MatchString(id) {
		http.NotFound(w, r)
		return
	}
	s.removeItem(id)
	s.libraryChanged()
	w.WriteHeader(http.StatusNoContent)
}

// Deletes every finished item of ?kind= (every kind when it is absent).
func (s *studio) handleClearLibrary(w http.ResponseWriter, r *http.Request) {
	items, err := s.items(r.URL.Query().Get("kind"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for _, item := range items {
		s.removeItem(item.ID)
	}
	s.libraryChanged()
	writeJSON(w, http.StatusOK, map[string]int{"deleted": len(items)})
}

func (s *studio) removeItem(id string) {
	sidecar := filepath.Join(s.dir, "library", id+".json")
	if raw, err := os.ReadFile(sidecar); err == nil {
		var item studioItem
		if json.Unmarshal(raw, &item) == nil {
			os.Remove(s.outputPath(&item))
		}
	}
	os.Remove(sidecar)
}

func (s *studio) libraryChanged() {
	s.mu.Lock()
	s.library++
	s.notifyLocked()
	s.mu.Unlock()
}

type studioRequest struct {
	Kind     string   `json:"kind"`
	Model    string   `json:"model"`
	Prompt   string   `json:"prompt"`
	Negative string   `json:"negative"`
	Width    int      `json:"width"`
	Height   int      `json:"height"`
	Steps    int      `json:"steps"`
	Frames   int      `json:"frames"`
	Seed     int64    `json:"seed"`
	Refs     []string `json:"refs"`
	Voice    string   `json:"voice"`
	Lang     string   `json:"lang"`
}

func (s *studio) handleCreateJob(w http.ResponseWriter, r *http.Request) {
	var req studioRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "bad request body", http.StatusBadRequest)
		return
	}
	job, err := s.enqueue(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func (s *studio) enqueue(req studioRequest) (*studioJob, error) {
	if req.Kind == "" {
		req.Kind = "image"
	}
	kind, ok := s.kinds[req.Kind]
	if !ok {
		return nil, fmt.Errorf("studio does not make %q", req.Kind)
	}
	req.Prompt = strings.TrimSpace(req.Prompt)
	req.Negative = strings.TrimSpace(req.Negative)
	if req.Prompt == "" {
		return nil, errors.New("describe it first")
	}
	if len(req.Refs) > studioMaxRefs {
		return nil, fmt.Errorf("at most %d reference files", studioMaxRefs)
	}
	spec := modelrt.Model(req.Model)
	if spec == nil || spec.Kind != kind.modelKind {
		return nil, fmt.Errorf("%q is not one of your %s models", req.Model, kind.modelKind)
	}
	if !modelrt.Installed(s.root, spec) {
		return nil, fmt.Errorf("%s is not installed — `fornax pull %s`", spec.ID, spec.ID)
	}
	for _, ref := range append(slices.Clone(req.Refs), req.Voice) {
		if ref != "" && s.refPath(ref) == "" {
			return nil, fmt.Errorf("reference %q is gone — add it again", ref)
		}
	}
	if req.Seed < 0 {
		n, err := rand.Int(rand.Reader, big.NewInt(1<<31))
		if err != nil {
			return nil, err
		}
		req.Seed = n.Int64()
	}
	if err := kind.validate(&req); err != nil {
		return nil, err
	}
	job := &studioJob{
		studioItem: studioItem{
			ID: newStudioID(), Kind: req.Kind, Ext: kind.ext, Model: spec.ID,
			Prompt: req.Prompt, Negative: req.Negative,
			Width: req.Width, Height: req.Height, Steps: req.Steps, Frames: req.Frames, Seed: req.Seed,
			Refs: req.Refs, Voice: req.Voice, Lang: req.Lang,
			Created: time.Now().UTC(),
		},
		State: jobQueued,
		Total: req.Steps,
	}
	s.mu.Lock()
	s.jobs = append(s.jobs, job)
	s.notifyLocked()
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
	return job, nil
}

// Cancels a queued or running job, or dismisses a failed one.
func (s *studio) handleCancelJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, job := range s.jobs {
		if job.ID != id {
			continue
		}
		if job.State == jobRunning {
			job.State = jobCanceled
			job.cancel()
		} else {
			s.jobs = slices.Delete(s.jobs, i, i+1)
		}
		s.notifyLocked()
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.NotFound(w, r)
}

// Reference files: images for edit models and image-to-video, audio clips
// for voice cloning and for audio models to hear.
var studioRefTypes = map[string]string{
	"image/png": ".png", "image/jpeg": ".jpg", "image/webp": ".webp",
	"audio/wave": ".wav", "audio/mpeg": ".mp3", "audio/ogg": ".ogg",
}

func (s *studio) handleUploadRef(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, studioMaxBytes))
	if err != nil {
		http.Error(w, "file is larger than 25 MB", http.StatusRequestEntityTooLarge)
		return
	}
	ext := studioRefTypes[http.DetectContentType(body)]
	if ext == "" {
		http.Error(w, "references must be PNG, JPEG, WebP, WAV, MP3 or Ogg", http.StatusUnsupportedMediaType)
		return
	}
	name := newStudioID() + ext
	if err := os.WriteFile(filepath.Join(s.dir, "refs", name), body, 0o600); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"name": name})
}

func (s *studio) refPath(name string) string {
	if !studioFilePattern.MatchString(name) {
		return ""
	}
	path := filepath.Join(s.dir, "refs", name)
	if _, err := os.Stat(path); err != nil {
		return ""
	}
	return path
}

var (
	studioIDPattern   = regexp.MustCompile(`^[0-9a-f]{16}$`)
	studioFilePattern = regexp.MustCompile(`^[0-9a-f]{16}\.(png|jpg|webp|webm|wav|mp3|ogg)$`)
)

func (s *studio) handleFile(w http.ResponseWriter, r *http.Request) {
	dir, name := r.PathValue("dir"), r.PathValue("name")
	if !slices.Contains([]string{"library", "refs", "previews"}, dir) || !studioFilePattern.MatchString(name) {
		http.NotFound(w, r)
		return
	}
	if dir == "previews" {
		w.Header().Set("Cache-Control", "no-store")
	} else {
		w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	}
	http.ServeFile(w, r, filepath.Join(s.dir, dir, name))
}

type studioSnapshot struct {
	Jobs      []studioJob   `json:"jobs"`
	Library   int           `json:"library"`
	Downloads []hubDownload `json:"downloads"`
}

func (s *studio) snapshotLocked() studioSnapshot {
	snap := studioSnapshot{Jobs: []studioJob{}, Library: s.library, Downloads: []hubDownload{}}
	for _, job := range s.jobs {
		snap.Jobs = append(snap.Jobs, *job)
	}
	for _, d := range s.downloads {
		snap.Downloads = append(snap.Downloads, *d)
	}
	return snap
}

// Server-sent events: the whole queue on connect and after every change.
// The queue is a handful of jobs, so a full snapshot beats a diff protocol.
func (s *studio) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	for {
		s.mu.Lock()
		snap, changed := s.snapshotLocked(), s.changed
		s.mu.Unlock()
		raw, _ := json.Marshal(snap)
		fmt.Fprintf(w, "data: %s\n\n", raw)
		flusher.Flush()
	wait:
		select {
		case <-r.Context().Done():
			return
		case <-changed:
		case <-ping.C:
			io.WriteString(w, ": ping\n\n")
			flusher.Flush()
			goto wait
		}
	}
}

func (s *studio) notifyLocked() {
	close(s.changed)
	s.changed = make(chan struct{})
}

// The queue runs one job at a time until ctx ends.
func (s *studio) work(ctx context.Context) {
	for {
		job, jobCtx := s.next(ctx)
		if job == nil {
			select {
			case <-ctx.Done():
				return
			case <-s.wake:
			}
			continue
		}
		err := s.generate(jobCtx, job)
		s.finish(job, err)
	}
}

func (s *studio) reapSD(ctx context.Context) {
	tick := time.NewTicker(chatReapEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			s.sd.stop()
			return
		case <-tick.C:
			s.sd.reapIdle()
		}
	}
}

func (s *studio) next(ctx context.Context) (*studioJob, context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, job := range s.jobs {
		if job.State == jobQueued {
			jobCtx, cancel := context.WithCancel(ctx)
			job.State, job.Phase, job.Started, job.cancel = jobRunning, "preparing", time.Now(), cancel
			s.notifyLocked()
			return job, jobCtx
		}
	}
	return nil, nil
}

func (s *studio) finish(job *studioJob, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	job.cancel()
	os.Remove(s.previewPath(&job.studioItem))
	if err == nil && job.State == jobRunning {
		job.Seconds = time.Since(job.Started).Seconds()
		err = s.saveSidecar(job)
		if err == nil {
			s.jobs = slices.DeleteFunc(s.jobs, func(j *studioJob) bool { return j == job })
			s.library++
			s.notifyLocked()
			return
		}
	}
	os.Remove(s.outputPath(&job.studioItem))
	if job.State == jobCanceled {
		s.jobs = slices.DeleteFunc(s.jobs, func(j *studioJob) bool { return j == job })
	} else {
		job.State, job.Error = jobFailed, err.Error()
	}
	s.notifyLocked()
}

func (s *studio) saveSidecar(job *studioJob) error {
	raw, err := json.MarshalIndent(job.studioItem, "", "  ")
	if err != nil {
		return err
	}
	return paths.AtomicPrivate(filepath.Join(s.dir, "library", job.ID+".json"), raw)
}

// Where a generation writes its file, and where the library serves it from.
func (s *studio) outputPath(item *studioItem) string {
	return filepath.Join(s.dir, "library", item.ID+"."+item.Ext)
}

func (s *studio) setPhase(job *studioJob, phase string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if job.Phase != phase {
		job.Phase = phase
		s.notifyLocked()
	}
}

func newStudioID() string {
	var raw [8]byte
	rand.Read(raw[:])
	return hex.EncodeToString(raw[:])
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(value)
}
