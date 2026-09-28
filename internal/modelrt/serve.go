package modelrt

// `run` and `connect`: pulling weights and running `llama-server` in the
// foreground — spawn with a scrubbed environment, prove `/health` and
// `/v1/models`, then babysit until Ctrl-C or the child exits.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/earshot-run/fornax/internal/catalog"
	"github.com/earshot-run/fornax/internal/download"
	"github.com/earshot-run/fornax/internal/engine"
	"github.com/earshot-run/fornax/internal/events"
	"github.com/earshot-run/fornax/internal/openai"
	"github.com/earshot-run/fornax/internal/paths"
	"github.com/earshot-run/fornax/internal/ui"
)

const (
	loadTimeout = 15 * time.Minute
	readyPoll   = 500 * time.Millisecond
)

// Download every file a model needs and promote each into place.
// Resumable: a `.part` keeps its bytes between runs.
func ensureModel(ctx context.Context, root string, spec *catalog.Spec, progress func(int64)) error {
	if spec.Runtime == catalog.Kev {
		return ensureKevModel(ctx, root, spec, progress)
	}
	if spec.Runtime == catalog.Apple {
		return ensureAppleModel(ctx, root, spec)
	}
	// progress counts the whole model, so each file reports on top of the
	// ones before it.
	var offset int64
	for _, file := range spec.Files() {
		if !FileInstalled(root, spec, file) {
			size, err := fetchFile(ctx, root, spec, file, func(n int64) { progress(offset + n) })
			if err != nil {
				return err
			}
			file.Bytes = size
		}
		offset += file.Bytes
		progress(offset)
	}
	return nil
}

func FileInstalled(root string, spec *catalog.Spec, file *catalog.Artifact) bool {
	info, err := os.Stat(paths.FilePath(root, spec, file))
	return err == nil && !info.IsDir()
}

// Download one file to its .part and promote it; the size comes from the
// server, so a file that changed upstream since it was listed still lands.
func fetchFile(ctx context.Context, root string, spec *catalog.Spec, file *catalog.Artifact, progress func(int64)) (int64, error) {
	part := paths.PartPath(root, spec, file)
	// Files can name nested paths (laya's tokenizer/, encoder/ dirs).
	if err := paths.ProtectDir(filepath.Dir(part)); err != nil {
		return 0, err
	}
	url := spec.URL(file)
	size, err := download.Size(ctx, url)
	if err != nil {
		return 0, err
	}
	if info, err := os.Stat(part); err == nil && size > 0 && info.Size() > size {
		os.Remove(part)
	}
	if err := download.Fetch(ctx, url, size, part, progress); err != nil {
		return 0, err
	}
	final := paths.FilePath(root, spec, file)
	os.Remove(final)
	if err := os.Rename(part, final); err != nil {
		return 0, fmt.Errorf("could not install model: %w", err)
	}
	if err := verifyDigest(final, file.SHA256); err != nil {
		os.Remove(final)
		return 0, err
	}
	if info, err := os.Stat(final); err == nil {
		size = info.Size()
	}
	return size, nil
}

// Check a downloaded file against the sha256 its source published. An empty
// want means the source publishes none (a GitHub release asset, a file
// without LFS metadata) and nothing is checked.
func verifyDigest(path, want string) error {
	if want == "" {
		return nil
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return fmt.Errorf("could not read %s back to verify it: %w", filepath.Base(path), err)
	}
	if got := hex.EncodeToString(hash.Sum(nil)); got != want {
		return fmt.Errorf("%s did not match its published sha256 (got %s…, want %s…) — the download is corrupt or the source changed; pull again", filepath.Base(path), got[:12], want[:12])
	}
	return nil
}

// The kev checkpoint: fetch the tarball, keep it, unpack the kev-<name>/
// dir it contains.
func ensureKevModel(ctx context.Context, root string, spec *catalog.Spec, progress func(int64)) error {
	file := &spec.Model
	if !FileInstalled(root, spec, file) {
		size, err := fetchFile(ctx, root, spec, file, progress)
		if err != nil {
			return err
		}
		file.Bytes = size
	}
	progress(file.Bytes)
	if KevCkptDir(root, spec) != "" {
		return nil
	}
	staging, err := os.MkdirTemp(root, ".ckpt-")
	if err != nil {
		return fmt.Errorf("could not stage the checkpoint: %w", err)
	}
	defer os.RemoveAll(staging)
	if err := engine.UnpackTarGz(paths.FilePath(root, spec, file), staging); err != nil {
		return err
	}
	entries, err := os.ReadDir(staging)
	if err != nil || len(entries) != 1 || !entries[0].IsDir() {
		return fmt.Errorf("the kev checkpoint archive has an unexpected layout")
	}
	if _, err := os.Stat(filepath.Join(staging, entries[0].Name(), "head.pt")); err != nil {
		return fmt.Errorf("the kev checkpoint archive is missing head.pt")
	}
	dest := filepath.Join(paths.ModelDir(root, spec), entries[0].Name())
	os.RemoveAll(dest)
	if err := os.Rename(filepath.Join(staging, entries[0].Name()), dest); err != nil {
		return fmt.Errorf("could not install the checkpoint: %w", err)
	}
	return nil
}

// What every child process fornax starts runs with. Never inherit
// LLAMA_ARG_*, provider credentials, preload variables, or DYLD injection
// from the caller.
func scrubbedEnv(root string) []string {
	env := []string{
		"HOME=" + filepath.Join(root, "server-home"),
		"PATH=/usr/bin:/bin:/usr/sbin:/sbin",
	}
	if tmp := os.Getenv("TMPDIR"); tmp != "" {
		env = append(env, "TMPDIR="+tmp)
	}
	return env
}

// logPath "" inherits the terminal (the `run` case); a path redirects the
// server's chatter to a file so one-shot commands keep stdout clean. extra
// carries `run -- <flags>` through verbatim, after the model's own saved
// arguments, so a caller can override any of the defaults above.
func spawnServer(root string, eng *catalog.EngineSpec, spec *catalog.Spec, port int, ctxSize int, logPath string, extra []string) (*exec.Cmd, error) {
	binary := paths.EngineBinary(root, eng, eng.Binary)
	args := []string{
		"--model", paths.ModelFinal(root, spec),
		"--alias", spec.ID,
		"--host", "127.0.0.1",
		"--port", fmt.Sprintf("%d", port),
		"--ctx-size", fmt.Sprintf("%d", ctxSize),
		"--parallel", "1",
		"--jinja",
		"--api-key-file", paths.KeyPath(root),
		"--no-ui",
		// Monotonic counters `run -idle` reads; behind the same key.
		"--metrics",
	}
	if spec.MMProj != nil {
		args = append(args, "--mmproj", paths.FilePath(root, spec, spec.MMProj))
	}
	if spec.Kind == catalog.Vision {
		// llama.cpp warns below 1024 on Qwen-VL grounding tasks.
		args = append(args, "--image-min-tokens", "1024")
	}
	if spec.Kind == catalog.Text {
		// Qwen3 emits thinking traces unless reasoning is off.
		args = append(args, "--reasoning", "off")
	}
	if spec.Kind == catalog.Embed {
		args = append(args, "--embeddings")
	}
	if spec.Kind == catalog.Rerank {
		args = append(args, "--reranking")
	}
	if runtime.GOOS != "windows" || eng.Backend != catalog.CPU {
		// The layer count is ignored where there is no offload backend.
		args = append(args, "--n-gpu-layers", "999")
	}
	// A model's saved arguments, then whatever `run --` passed: last wins.
	args = append(args, spec.Args...)
	args = append(args, extra...)
	cmd := exec.Command(binary, args...)
	cmd.Dir = filepath.Dir(binary)
	cmd.Env = engineEnv(root, binary)
	cmd.Stdin = nil
	if logPath != "" {
		log, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, fmt.Errorf("could not open %s: %w", logPath, err)
		}
		cmd.Stdout = log
		cmd.Stderr = log
	} else {
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("could not start llama-server: %w", err)
	}
	return cmd, nil
}

// Poll `/health` and `/v1/models` until the model is genuinely answering.
// kev has no /health — passes probeHealth=false and lists instead.
func waitReady(ctx context.Context, exited <-chan error, port int, alias, key string, probeHealth bool) error {
	client := &http.Client{
		Timeout:   3 * time.Second,
		Transport: &http.Transport{Proxy: nil},
	}
	started := time.Now()
	root := fmt.Sprintf("http://127.0.0.1:%d", port)
	for {
		select {
		case status := <-exited:
			return fmt.Errorf("the model server exited before it was ready (%v)", status)
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if time.Since(started) >= loadTimeout {
			return fmt.Errorf("the model did not load within %d minutes", int(loadTimeout.Minutes()))
		}
		healthy := !probeHealth
		if probeHealth {
			if health := openai.GetJSON(client, root+"/health", key); health != nil {
				healthy = health["status"] == "ok"
			}
		}
		listed := slices.Contains(servedIDs(client, fmt.Sprintf("http://127.0.0.1:%d", port), key), alias)
		if healthy && listed {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case status := <-exited:
			return fmt.Errorf("the model server exited before it was ready (%v)", status)
		case <-time.After(readyPoll):
		}
	}
}

// Model ids a server advertises, whichever listing shape it speaks:
// OpenAI `{"data":[{id}]}`, or kev `{"models":[{id, aliases}]}`.
func servedIDs(client *http.Client, base, key string) []string {
	models := openai.GetJSON(client, base+"/v1/models", key)
	if models == nil {
		return nil
	}
	var ids []string
	add := func(s string) {
		if s != "" && !slices.Contains(ids, s) {
			ids = append(ids, s)
		}
	}
	collect := func(rows any) {
		if list, ok := rows.([]any); ok {
			for _, row := range list {
				obj, ok := row.(map[string]any)
				if !ok {
					continue
				}
				for _, field := range []string{"id", "name", "model"} {
					if v, ok := obj[field].(string); ok {
						add(v)
					}
				}
				if aliases, ok := obj["aliases"].([]any); ok {
					for _, a := range aliases {
						if s, ok := a.(string); ok {
							add(s)
						}
					}
				}
			}
		}
	}
	collect(models["data"])
	collect(models["models"])
	return ids
}

// Whether the server on this model's own port is advertising it. kev answers
// under its family alias rather than its id.
func IsServing(spec *catalog.Spec, key string) bool {
	alias := spec.ID
	if spec.Runtime == catalog.Kev {
		alias = kevAlias
	}
	return slices.Contains(servedModels(spec.Port, key), alias)
}

// What the running server advertises, or nil when it is not up (or the key
// is not its key).
func servedModels(port int, key string) []string {
	client := &http.Client{
		Timeout:   3 * time.Second,
		Transport: &http.Transport{Proxy: nil},
	}
	return servedIDs(client, fmt.Sprintf("http://127.0.0.1:%d", port), key)
}

func portFree(port int) error {
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return fmt.Errorf("port %d is already in use", port)
	}
	return listener.Close()
}

// A loopback port nothing on this machine is listening on, starting at base.
func freePort(base int) (int, error) {
	for port := base; port < base+200; port++ {
		listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err == nil {
			listener.Close()
			return port, nil
		}
	}
	return 0, fmt.Errorf("no free loopback port near %d", base)
}

// run and connect both need a model with a server behind it. An image model
// serves OpenAI's images API; video and speech are foreground tools.
func requireServer(spec *catalog.Spec) error {
	if spec.Runtime == catalog.SD && spec.Kind == catalog.Image {
		return nil
	}
	if spec.Runtime != catalog.SD && spec.Kind != catalog.Speech {
		return nil
	}
	does, command, operands := Instead(spec)
	return fmt.Errorf("%s %s, it does not serve — `fornax %s %s %s`", spec.ID, does, command, spec.ID, operands)
}

// `fornax run`: pull, verify, spawn on the model's own port (or port), then
// hold until Ctrl-C, the idle limit, or the --events reader going away.
// extra is `run -- <flags>`, handed to llama-server unchanged. ctxSet marks
// an explicit -ctx-size, which is never lowered to the model's trained
// length (a user may run a short-context model with rope scaling).
func Serve(ctx context.Context, id string, port, ctxSize int, noConnect bool, idle time.Duration, extra []string, ctxSet bool) error {
	spec, eng, err := Resolve(ctx, id)
	if err != nil {
		return err
	}
	if err := requireServer(spec); err != nil {
		return err
	}
	servePort := spec.Port
	if port != 0 {
		servePort = port
	}
	root := paths.Home()
	if err := Pull(ctx, spec, eng); err != nil {
		return err
	}
	if spec.Runtime == catalog.SD {
		if ctxSize != catalog.ContextWindow {
			fmt.Fprintln(os.Stderr, "note: -ctx-size is ignored for image models")
		}
		if len(extra) > 0 {
			fmt.Fprintln(os.Stderr, "note: flags after -- are ignored for image models (they go to sd-cli, not sd-server)")
		}
		return serveSDImage(ctx, root, spec, eng, servePort, idle)
	}
	if spec.Runtime == catalog.Apple {
		return runApple(ctx, root, spec, servePort, ctxSize != catalog.ContextWindow, noConnect, idle)
	}
	if err := portFree(servePort); err != nil {
		return err
	}
	var cmd *exec.Cmd
	var key string
	serverName := "llama-server"
	logPath := ""
	if events.On() {
		logPath = paths.ServerLog(root)
	}
	events.Emit("stage", map[string]any{"stage": "loading", "model": spec.ID})
	switch spec.Runtime {
	case catalog.Kev, catalog.Laya:
		if spec.Runtime == catalog.Kev {
			serverName = "kev.serve"
		} else {
			serverName = "laya"
		}
		if ctxSize != catalog.ContextWindow {
			fmt.Fprintf(os.Stderr, "note: -ctx-size is ignored for %s models\n", spec.Runtime)
		}
		if noConnect {
			fmt.Fprintf(os.Stderr, "note: -no-connect is ignored for %s models (no Earshot route)\n", spec.Runtime)
		}
		if idle > 0 {
			fmt.Fprintf(os.Stderr, "note: -idle is ignored for %s models (no activity counters)\n", spec.Runtime)
			idle = 0
		}
		if len(extra) > 0 {
			fmt.Fprintf(os.Stderr, "note: flags after -- are ignored for %s models (they go to llama-server)\n", spec.Runtime)
		}
		key, err = paths.EnsureKey(root)
		if err == nil {
			if spec.Runtime == catalog.Kev {
				cmd, err = spawnKev(root, spec, servePort, key, logPath)
			} else {
				cmd, err = spawnLaya(root, spec, servePort, logPath)
			}
		}
	default:
		key, err = paths.EnsureKey(root)
		if err != nil {
			return err
		}
		if !ctxSet {
			if lowered, note := contextFor(root, spec, ctxSize); note != "" {
				ctxSize = lowered
				fmt.Fprintln(os.Stderr, ui.Dim(note))
			}
		} else if trained, ok := ModelContext(root, spec); ok && ctxSize > trained {
			fmt.Fprintln(os.Stderr, ui.Dim(fmt.Sprintf("note: %s was trained for %d tokens; %d may degrade its output", spec.ID, trained, ctxSize)))
		}
		cmd, err = spawnServer(root, eng, spec, servePort, ctxSize, logPath, extra)
	}
	if err != nil {
		return err
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	if spec.Runtime == catalog.Kev {
		err = waitReady(ctx, exited, servePort, kevAlias, key, false)
	} else {
		err = waitReady(ctx, exited, servePort, spec.ID, key, true)
	}
	if err != nil {
		killAndReap(cmd, exited)
		return err
	}
	probe := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{Proxy: nil}}
	return holdServing(ctx, &serving{
		spec: spec, port: servePort, key: key, noConnect: noConnect, idle: idle,
		process: serverName, exited: exited, stop: func() { killAndReap(cmd, exited) },
		sample: func() (string, bool) { return metricsFingerprint(probe, servePort, key) },
	})
}

func holdServing(ctx context.Context, s *serving) error {
	url := paths.EndpointURL(s.port)
	if events.On() {
		earshot, detail := "skipped", ""
		if s.spec.Kind != catalog.Decision && !s.noConnect {
			var result connectResult
			switch result, detail = connectEarshot(url, s.key); result {
			case connectRegistered:
				earshot = "connected"
			case connectUnavailable:
				earshot = "refused"
			default:
				earshot = "absent"
			}
		}
		events.Emit("ready", map[string]any{"model": s.spec.ID, "url": url, "port": s.port, "earshot": earshot, "detail": detail})
	} else {
		fmt.Printf("\n%s %s\n", ui.MarkOK(), ui.Bold(s.spec.Name)+" is serving")
		fmt.Printf("    %s %s\n", ui.Dim("url:"), ui.Cyan(url))
		switch {
		case s.panel != nil:
			s.panel(url, s.key)
		case s.spec.Kind == catalog.Decision:
			fmt.Println(sdkBlock(url, DecisionModel(s.spec), s.key))
		case s.noConnect:
			fmt.Println(pasteBlock(url, s.key, s.spec.ID))
		default:
			reportConnect(url, s.key, s.spec.ID)
		}
		if s.spec.Runtime == catalog.Llama && s.spec.Kind != catalog.Embed && s.spec.Kind != catalog.Rerank {
			fmt.Println(anthropicLine(url, s.key))
		}
		if s.idle > 0 {
			fmt.Println(ui.Dim(fmt.Sprintf("\nstops after %s without a request · ctrl-c to stop now", s.idle)))
		} else {
			fmt.Println(ui.Dim("\nctrl-c to stop"))
		}
	}
	done := make(chan struct{})
	defer close(done)
	go events.Heartbeat(done)
	select {
	case <-ctx.Done():
		fmt.Fprintln(os.Stderr, ui.Dim("stopping…"))
		s.stop()
		events.Emit("stopped", map[string]any{"model": s.spec.ID, "reason": "signal"})
		return nil
	case <-events.SupervisorGone():
		s.stop()
		return nil
	case <-idleAfter(s.idle, idlePoll, s.sample, done):
		fmt.Fprintln(os.Stderr, ui.Dim(fmt.Sprintf("no requests for %s — stopping", s.idle)))
		s.stop()
		events.Emit("stopped", map[string]any{"model": s.spec.ID, "reason": "idle"})
		return nil
	case status := <-s.exited:
		s.stop()
		return fmt.Errorf("%s exited: %v", s.process, status)
	}
}

func killAndReap(cmd *exec.Cmd, exited <-chan error) {
	cmd.Process.Kill()
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
	}
}

// Register a model already serving on port (0: its own) with Earshot, or
// print the block to paste when it has no Earshot route.
func Connect(id string, port int) error {
	spec := Model(id)
	if spec == nil {
		return UnknownModel(id)
	}
	if err := requireServer(spec); err != nil {
		return err
	}
	if spec.Runtime == catalog.SD {
		return fmt.Errorf("%s serves images, not chat — Earshot registers chat models. Point a client at %s/images/generations with the key `fornax run %s` prints",
			spec.ID, paths.EndpointURL(spec.Port), spec.ID)
	}
	servePort := spec.Port
	if port != 0 {
		servePort = port
	}
	root := paths.Home()
	cfg, err := paths.LoadConfig(root)
	if err != nil {
		return err
	}
	if cfg.APIKey == "" {
		return fmt.Errorf("no server key yet — run `fornax run %s` once first", spec.ID)
	}
	if spec.Kind == catalog.Decision {
		// Decision models have no Earshot route — print the SDK block.
		if slices.Contains(servedModels(servePort, cfg.APIKey), DecisionModel(spec)) {
			fmt.Println(sdkBlock(paths.EndpointURL(servePort), DecisionModel(spec), cfg.APIKey))
			return nil
		}
		return fmt.Errorf("nothing is answering on 127.0.0.1:%d — is `%s` running?", servePort, spec.ID)
	}
	served := servedModels(servePort, cfg.APIKey)
	switch {
	case served == nil:
		return fmt.Errorf("nothing is answering on 127.0.0.1:%d — is `%s` running?", servePort, spec.ID)
	case !slices.Contains(served, spec.ID):
		return fmt.Errorf("127.0.0.1:%d serves [%s], not %s", servePort, strings.Join(served, ", "), spec.ID)
	}
	url := paths.EndpointURL(servePort)
	result, detail := connectEarshot(url, cfg.APIKey)
	switch result {
	case connectRegistered:
		fmt.Printf("%s %s connected — %s is in Settings ▸ Local models\n", ui.MarkOK(), ui.Green("earshot:"), ui.Bold(spec.ID))
		return nil
	case connectUnavailable:
		fmt.Println(pasteBlock(url, cfg.APIKey, spec.ID))
		return fmt.Errorf("earshot would not connect: %s", detail)
	default:
		return fmt.Errorf("no Earshot daemon on this computer\n%s", pasteBlock(url, cfg.APIKey, spec.ID))
	}
}

// A model that is up: announce it, then hold until something ends it.
type serving struct {
	spec      *catalog.Spec
	port      int
	key       string
	noConnect bool
	idle      time.Duration
	process   string
	exited    <-chan error
	stop      func()
	sample    func() (string, bool)
	// Replaces the chat connect block when set (image models).
	panel func(url, key string)
}
