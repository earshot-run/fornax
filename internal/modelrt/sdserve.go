package modelrt

// `run` for an image model: sd-server holds the weights, and a small loopback
// adapter in front binds the model's port, enforces the generated key, and
// speaks OpenAI's images API — /v1/images/generations and /v1/images/edits —
// translating to sd-server's native job API. sd-server has no key of its own
// and stays on a scratch port. Video stays on `animate`: OpenAI has no video
// route, and a clip takes minutes.

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/earshot-run/fornax/internal/catalog"
	"github.com/earshot-run/fornax/internal/events"
	"github.com/earshot-run/fornax/internal/openai"
	"github.com/earshot-run/fornax/internal/paths"
	"github.com/earshot-run/fornax/internal/ui"
)

// One request makes at most this many images, so a stray n cannot pin the GPU.
const maxImagesPerRequest = 8

// Where `url` responses land, served back at /v1/files/<name>.
const imageNamePattern = `^[0-9a-f]{16}\.png$`

var imageNameRe = regexp.MustCompile(imageNamePattern)

// Serve an image model on port until ctx ends: sd-server behind a keyed,
// OpenAI-shaped adapter. holdServing supplies the --events stream, the idle
// stop and the supervisor leash.
func serveSDImage(ctx context.Context, root string, spec *catalog.Spec, eng *catalog.EngineSpec, port int, idle time.Duration) error {
	key, err := paths.EnsureKey(root)
	if err != nil {
		return err
	}
	if err := portFree(port); err != nil {
		return err
	}
	dir := filepath.Join(root, "images")
	if err := paths.ProtectDir(dir); err != nil {
		return err
	}
	events.Emit("stage", map[string]any{"stage": "loading", "model": spec.ID})
	var log *os.File
	echo := func(line string) {
		if log != nil {
			fmt.Fprintln(log, line)
			return
		}
		fmt.Fprintln(os.Stderr, ui.Dim(line))
	}
	if events.On() {
		log, _ = os.OpenFile(paths.ServerLog(root), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	}
	defer func() {
		if log != nil {
			log.Close()
		}
	}()
	server, err := StartSD(ctx, root, eng, spec, echo)
	if err != nil {
		return fmt.Errorf("%w — `fornax imagine %s \"…\"` runs the model through sd-cli instead", err, spec.ID)
	}
	defer server.Close()

	a := &sdImageServer{root: root, spec: spec, key: key, server: server, dir: dir, port: port}
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return fmt.Errorf("port %d is already in use", port)
	}
	httpSrv := &http.Server{
		Handler:           a.handler(),
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		httpSrv.Shutdown(shutdown)
	}()
	go httpSrv.Serve(listener)

	return holdServing(ctx, &serving{
		spec: spec, port: port, key: key, noConnect: true, idle: idle,
		process: "sd-server", exited: server.Exited(),
		stop:   func() { httpSrv.Close(); server.Close() },
		sample: func() (string, bool) { return strconv.FormatInt(a.requests.Load(), 10), true },
		panel:  a.panel,
	})
}

type sdImageServer struct {
	root   string
	spec   *catalog.Spec
	key    string
	server *SDServer
	dir    string
	port   int
	// Image requests served, for `run -idle`; /v1/models probes do not count.
	requests atomic.Int64
}

func (a *sdImageServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
	})
	mux.HandleFunc("GET /v1/models", a.auth(a.models))
	mux.HandleFunc("POST /v1/images/generations", a.auth(a.generations))
	mux.HandleFunc("POST /v1/images/edits", a.auth(a.edits))
	mux.HandleFunc("GET /v1/files/{name}", a.auth(a.file))
	return mux
}

// The generated key, as `Authorization: Bearer` or `x-api-key`.
func (a *sdImageServer) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		given := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if given == "" {
			given = r.Header.Get("x-api-key")
		}
		if subtle.ConstantTimeCompare([]byte(given), []byte(a.key)) != 1 {
			imageError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next(w, r)
	}
}

// The listing shape servedIDs reads, so `ps` and probes see the model.
func (a *sdImageServer) models(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   []any{map[string]any{"id": a.spec.ID, "object": "model", "owned_by": "fornax"}},
	})
}

func (a *sdImageServer) file(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !imageNameRe.MatchString(name) {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	http.ServeFile(w, r, filepath.Join(a.dir, name))
}

// The OpenAI images request, plus the fields a local model understands:
// seed, steps, a negative prompt, and an image to edit from.
type imageRequest struct {
	Prompt         string `json:"prompt"`
	Model          string `json:"model"`
	N              int    `json:"n"`
	Size           string `json:"size"`
	ResponseFormat string `json:"response_format"`
	Seed           *int64 `json:"seed"`
	Steps          int    `json:"steps"`
	Negative       string `json:"negative_prompt"`
	NegativeAlt    string `json:"negative"`
	Image          string `json:"image"`
}

func (a *sdImageServer) generations(w http.ResponseWriter, r *http.Request) {
	var req imageRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		imageError(w, http.StatusBadRequest, "bad request body")
		return
	}
	var refs []string
	if strings.TrimSpace(req.Image) != "" {
		path, err := a.saveImage(req.Image)
		if err != nil {
			imageError(w, http.StatusBadRequest, err.Error())
			return
		}
		refs = append(refs, path)
	}
	a.generate(w, r, &req, refs)
}

// /v1/images/edits: OpenAI sends multipart with an `image` file; a JSON body
// with a data URL or path is accepted too.
func (a *sdImageServer) edits(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			imageError(w, http.StatusBadRequest, "bad multipart body")
			return
		}
		req := imageRequest{
			Prompt:         r.FormValue("prompt"),
			Size:           r.FormValue("size"),
			ResponseFormat: r.FormValue("response_format"),
			Negative:       r.FormValue("negative_prompt"),
			Steps:          atoiOr(r.FormValue("steps"), 0),
		}
		if seed := r.FormValue("seed"); seed != "" {
			if n, err := strconv.ParseInt(seed, 10, 64); err == nil {
				req.Seed = &n
			}
		}
		file, _, err := r.FormFile("image")
		if err != nil {
			imageError(w, http.StatusBadRequest, "edits wants an image file")
			return
		}
		defer file.Close()
		path, err := a.storeImage(file)
		if err != nil {
			imageError(w, http.StatusBadRequest, err.Error())
			return
		}
		a.generate(w, r, &req, []string{path})
		return
	}
	var req imageRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		imageError(w, http.StatusBadRequest, "bad request body")
		return
	}
	if strings.TrimSpace(req.Image) == "" {
		imageError(w, http.StatusBadRequest, "edits wants an image")
		return
	}
	path, err := a.saveImage(req.Image)
	if err != nil {
		imageError(w, http.StatusBadRequest, err.Error())
		return
	}
	a.generate(w, r, &req, []string{path})
}

func (a *sdImageServer) generate(w http.ResponseWriter, r *http.Request, req *imageRequest, refs []string) {
	if strings.TrimSpace(req.Prompt) == "" {
		imageError(w, http.StatusBadRequest, "prompt is required")
		return
	}
	width, height := 0, 0
	if size := strings.ToLower(strings.TrimSpace(req.Size)); size != "" && size != "auto" {
		var err error
		if width, height, err = parseImageSize(size); err != nil {
			imageError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	n := req.N
	if n < 1 {
		n = 1
	}
	n = min(n, maxImagesPerRequest)
	seed := int64(-1)
	if req.Seed != nil {
		seed = *req.Seed
	}
	negative := req.Negative
	if negative == "" {
		negative = req.NegativeAlt
	}
	a.requests.Add(1)
	data := make([]map[string]any, 0, n)
	for range n {
		out := filepath.Join(a.dir, imageID()+".png")
		sreq := SDRequest{
			Prompt: req.Prompt, Negative: negative,
			Width: width, Height: height, Steps: req.Steps, Seed: seed, Refs: refs,
		}
		if err := a.server.Generate(r.Context(), sreq, out); err != nil {
			if r.Context().Err() != nil {
				return // the client went away; the job was cancelled
			}
			os.Remove(out)
			imageError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if req.ResponseFormat == "url" {
			data = append(data, map[string]any{
				"url": fmt.Sprintf("http://127.0.0.1:%d/v1/files/%s", a.port, filepath.Base(out)),
			})
			continue
		}
		raw, err := os.ReadFile(out)
		os.Remove(out) // a b64 response leaves no file behind
		if err != nil {
			imageError(w, http.StatusInternalServerError, err.Error())
			return
		}
		data = append(data, map[string]any{"b64_json": base64.StdEncoding.EncodeToString(raw)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"created": time.Now().Unix(), "data": data})
}

// An image from a data URL, a readable path, or raw base64.
func (a *sdImageServer) saveImage(value string) (string, error) {
	value = strings.TrimSpace(value)
	if rest, ok := strings.CutPrefix(value, "data:"); ok {
		_, b64, ok := strings.Cut(rest, ";base64,")
		if !ok {
			return "", fmt.Errorf("image data URL is not base64")
		}
		raw, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return "", fmt.Errorf("image data URL is not readable")
		}
		return a.writeImage(raw)
	}
	if info, err := os.Stat(value); err == nil && !info.IsDir() {
		raw, err := os.ReadFile(value)
		if err != nil {
			return "", fmt.Errorf("could not read %s", value)
		}
		return a.writeImage(raw)
	}
	if raw, err := base64.StdEncoding.DecodeString(value); err == nil {
		return a.writeImage(raw)
	}
	return "", fmt.Errorf("image is not a data URL, base64 or a readable path")
}

func (a *sdImageServer) storeImage(r io.Reader) (string, error) {
	raw, err := io.ReadAll(io.LimitReader(r, 32<<20))
	if err != nil || len(raw) == 0 {
		return "", fmt.Errorf("could not read the uploaded image")
	}
	return a.writeImage(raw)
}

func (a *sdImageServer) writeImage(raw []byte) (string, error) {
	path := filepath.Join(a.dir, imageID()+".png")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// What `run` prints for an image model, in place of the chat connect block.
func (a *sdImageServer) panel(url, key string) {
	fmt.Printf("    %s %s\n", ui.Dim("images:"), ui.Dim("POST "+url+"/images/generations"))
	fmt.Printf("    %s %s\n", ui.Dim("key:"), key)
	fmt.Printf("    %s\n", ui.Dim(fmt.Sprintf(
		`curl -s %s/images/generations -H "Authorization: Bearer %s" -H "Content-Type: application/json" -d '{"prompt":"a red fox in snow"}'`,
		url, key)))
}

// WxH within the engine's bounds; "auto" and "" leave the model's own size.
func parseImageSize(size string) (int, int, error) {
	w, h, ok := strings.Cut(strings.ToLower(strings.TrimSpace(size)), "x")
	width, errW := strconv.Atoi(strings.TrimSpace(w))
	height, errH := strconv.Atoi(strings.TrimSpace(h))
	if !ok || errW != nil || errH != nil || width < 64 || height < 64 || width > 4096 || height > 4096 {
		return 0, 0, fmt.Errorf("size wants WxH between 64 and 4096, got %q", size)
	}
	return width, height, nil
}

func atoiOr(value string, fallback int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(value)); err == nil {
		return n
	}
	return fallback
}

func imageID() string {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return fmt.Sprintf("%016x", time.Now().UnixNano())
	}
	return hex.EncodeToString(raw[:])
}

func imageError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": message}})
}

// ServedImage runs one image generation against a model's `run` endpoint when
// one is serving, so a client that keeps a server up pays no model reload.
// handled is false when nothing is serving and the caller should fall back to
// sd-cli. A true handled with a nil error means outPath was written.
func ServedImage(ctx context.Context, spec *catalog.Spec, req SDRequest, outPath string) (bool, error) {
	root := paths.Home()
	key, err := paths.EnsureKey(root)
	if err != nil {
		return false, err
	}
	if !IsServing(spec, key) {
		return false, nil
	}
	body := map[string]any{"prompt": req.Prompt, "response_format": "b64_json"}
	if req.Negative != "" {
		body["negative_prompt"] = req.Negative
	}
	if req.Width > 0 && req.Height > 0 {
		body["size"] = fmt.Sprintf("%dx%d", req.Width, req.Height)
	}
	if req.Steps > 0 {
		body["steps"] = req.Steps
	}
	if req.Seed >= 0 {
		body["seed"] = req.Seed
	}
	if len(req.Refs) > 0 {
		body["image"] = req.Refs[0]
	}
	raw, _ := json.Marshal(body)
	resp, err := openai.Post(ctx, paths.EndpointURL(spec.Port)+"/images/generations", key, raw)
	if err != nil {
		return true, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return true, openai.Error(resp)
	}
	var parsed struct {
		Data []struct {
			B64 string `json:"b64_json"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 128<<20)).Decode(&parsed); err != nil {
		return true, fmt.Errorf("the image server's reply was not readable: %w", err)
	}
	if len(parsed.Data) == 0 || parsed.Data[0].B64 == "" {
		return true, fmt.Errorf("the image server returned no image")
	}
	image, err := base64.StdEncoding.DecodeString(parsed.Data[0].B64)
	if err != nil {
		return true, fmt.Errorf("the image server sent an unreadable image: %w", err)
	}
	abs, err := filepath.Abs(outPath)
	if err != nil {
		return true, err
	}
	if err := os.WriteFile(abs, image, 0o644); err != nil {
		return true, err
	}
	return true, nil
}
