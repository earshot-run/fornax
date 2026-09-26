package main

// `run` and `connect`: pulling weights and running `llama-server` in the
// foreground — spawn with a scrubbed environment, prove `/health` and
// `/v1/models`, then babysit until Ctrl-C or the child exits.

import (
	"context"
	"encoding/json"
	"flag"
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

// Download every artifact a model needs, verify each, promote into place.
// Resumable: a `.part` keeps its bytes between runs.
func ensureModel(ctx context.Context, root string, spec *catalog.Spec, progress func(int64)) error {
	if spec.Runtime == catalog.Kev {
		return ensureKevModel(ctx, root, spec, progress)
	}
	if spec.Runtime == catalog.Apple {
		return ensureAppleModel(ctx, root, spec)
	}
	for _, pin := range spec.Files() {
		installed := fileInstalled(root, spec, pin)
		if installed {
			progress(pin.Bytes)
			continue
		}
		part, err := prepareCandidate(root, spec, pin)
		if err != nil {
			return err
		}
		if err := download.Fetch(ctx, spec.URL(pin), pin.Bytes, part, progress); err != nil {
			return err
		}
		if err := download.Verify(part, pin.Bytes, pin.SHA256); err != nil {
			return err
		}
		if err := promote(root, spec, pin, part); err != nil {
			return err
		}
	}
	return paths.WriteReceipt(paths.ModelDir(root, spec), spec)
}

func fileInstalled(root string, spec *catalog.Spec, pin *catalog.Pin) bool {
	info, err := os.Stat(paths.FilePath(root, spec, pin))
	return err == nil && info.Size() == pin.Bytes
}

func prepareCandidate(root string, spec *catalog.Spec, pin *catalog.Pin) (string, error) {
	// Pins can name nested paths (laya's tokenizer/, encoder/ dirs).
	if err := paths.ProtectDir(filepath.Dir(paths.PartPath(root, spec, pin))); err != nil {
		return "", err
	}
	part := paths.PartPath(root, spec, pin)
	final := paths.FilePath(root, spec, pin)
	// A verified-but-unpromoted file from an earlier crash resumes as a part.
	if _, err := os.Stat(part); os.IsNotExist(err) {
		if info, err := os.Stat(final); err == nil && info.Size() == pin.Bytes {
			if err := os.Rename(final, part); err != nil {
				return "", fmt.Errorf("could not resume model verification: %w", err)
			}
		}
	}
	if info, err := os.Stat(part); err == nil && info.Size() > pin.Bytes {
		os.Remove(part)
	}
	return part, nil
}

func promote(root string, spec *catalog.Spec, pin *catalog.Pin, part string) error {
	final := paths.FilePath(root, spec, pin)
	os.Remove(final)
	if err := os.Rename(part, final); err != nil {
		return fmt.Errorf("could not install model: %w", err)
	}
	return nil
}

// The kev checkpoint: fetch the verified tarball, keep it (re-hash before
// every spawn reads it), unpack the kev-<name>/ dir it contains.
func ensureKevModel(ctx context.Context, root string, spec *catalog.Spec, progress func(int64)) error {
	pin := &spec.Model
	if !fileInstalled(root, spec, pin) {
		part, err := prepareCandidate(root, spec, pin)
		if err != nil {
			return err
		}
		if err := download.Fetch(ctx, spec.URL(pin), pin.Bytes, part, progress); err != nil {
			return err
		}
		if err := download.Verify(part, pin.Bytes, pin.SHA256); err != nil {
			return err
		}
		if err := promote(root, spec, pin, part); err != nil {
			return err
		}
	}
	progress(pin.Bytes)
	if kevCkptDir(root, spec) != "" {
		return paths.WriteReceipt(paths.ModelDir(root, spec), spec)
	}
	staging, err := os.MkdirTemp(root, ".ckpt-")
	if err != nil {
		return fmt.Errorf("could not stage the checkpoint: %w", err)
	}
	defer os.RemoveAll(staging)
	if err := engine.UnpackTarGz(paths.FilePath(root, spec, pin), staging); err != nil {
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
	return paths.WriteReceipt(paths.ModelDir(root, spec), spec)
}

// Re-hash every pinned file before each spawn: the install receipt cannot
// authorize bytes that may have changed since.
func rehash(root string, spec *catalog.Spec) error {
	for _, pin := range spec.Files() {
		if err := download.Verify(paths.FilePath(root, spec, pin), pin.Bytes, pin.SHA256); err != nil {
			return err
		}
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
// server's chatter to a file so one-shot commands keep stdout clean.
func spawnServer(root string, eng *catalog.EngineSpec, spec *catalog.Spec, port int, ctxSize int, logPath string) (*exec.Cmd, error) {
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
	if runtime.GOOS != "windows" {
		// The layer count is ignored where there is no offload backend.
		args = append(args, "--n-gpu-layers", "999")
	}
	cmd := exec.Command(binary, args...)
	cmd.Dir = filepath.Dir(binary)
	cmd.Env = scrubbedEnv(root)
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
			if health := authenticatedGet(client, root+"/health", key); health != nil {
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
	models := authenticatedGet(client, base+"/v1/models", key)
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

func authenticatedGet(client *http.Client, url, key string) map[string]any {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, openai.MaxBody))
	if err != nil {
		return nil
	}
	var value map[string]any
	if json.Unmarshal(body, &value) != nil {
		return nil
	}
	return value
}

// Whether the server on this model's own port is advertising it. kev answers
// under its family alias and without a key.
func isServing(spec *catalog.Spec, key string) bool {
	alias := spec.ID
	if spec.Runtime == catalog.Kev {
		alias, key = kevAlias, ""
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

// run and connect both need a model with an OpenAI-compatible server behind it.
func requireServer(spec *catalog.Spec) error {
	if spec.Runtime != catalog.SD && spec.Kind != catalog.Speech {
		return nil
	}
	does, command, operands := instead(spec)
	return fmt.Errorf("%s %s, it does not serve — `fornax %s %s %s`", spec.ID, does, command, spec.ID, operands)
}

func cmdRun(ctx context.Context, args []string) error {
	set := flag.NewFlagSet("run", flag.ExitOnError)
	port := set.Int("port", 0, "loopback port to serve on (default: the model's assigned port)")
	ctxSize := set.Int("ctx-size", catalog.ContextWindow, "context window passed to llama-server")
	noConnect := set.Bool("no-connect", false, "do not register the running server with Earshot")
	idle := set.Duration("idle", 0, "stop after this long without a request, e.g. 20m (default: never)")
	asEvents := set.Bool("events", false, "one JSON event per line on stdout; stops when the reader goes away")
	set.Usage = ui.UsageFunc(set, "usage: fornax run <model> [-port N] [-ctx-size N] [-idle 20m] [-no-connect] [--events]")
	got := parseFlexible(set, args, 1)
	if len(got) != 1 {
		return fmt.Errorf("usage: fornax run <model> [-port N] [-ctx-size N] [-idle 20m] [-no-connect] [--events]")
	}
	id := got[0]
	if *asEvents {
		events.Enable()
	}
	err := serveModel(ctx, id, *port, *ctxSize, *noConnect, *idle)
	if err != nil {
		events.Emit("error", map[string]any{"message": err.Error()})
	}
	return err
}

func serveModel(ctx context.Context, id string, port, ctxSize int, noConnect bool, idle time.Duration) error {
	spec, eng, err := resolve(ctx, id)
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
	if err := pull(ctx, spec, eng); err != nil {
		return err
	}
	if spec.Runtime == catalog.Apple {
		return runApple(ctx, root, spec, servePort, ctxSize != catalog.ContextWindow, noConnect, idle)
	}
	events.Emit("stage", map[string]any{"stage": "verifying", "model": spec.ID})
	verifying := ui.Spin("verifying " + spec.ID)
	if err := rehash(root, spec); err != nil {
		verifying.Stop("")
		return err
	}
	verifying.Stop("")
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
		if spec.Runtime == catalog.Kev {
			cmd, err = spawnKev(root, spec, servePort, logPath)
		} else {
			key, err = paths.EnsureKey(root)
			if err == nil {
				cmd, err = spawnLaya(root, spec, servePort, logPath)
			}
		}
	default:
		key, err = paths.EnsureKey(root)
		if err != nil {
			return err
		}
		cmd, err = spawnServer(root, eng, spec, servePort, ctxSize, logPath)
	}
	if err != nil {
		return err
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	if spec.Runtime == catalog.Kev {
		err = waitReady(ctx, exited, servePort, kevAlias, "", false)
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
		case s.spec.Runtime == catalog.Kev:
			fmt.Println(kevBlock(url))
		case s.spec.Runtime == catalog.Laya:
			fmt.Println(layaBlock(url, s.spec.ID, s.key))
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

func cmdConnect(args []string) error {
	set := flag.NewFlagSet("connect", flag.ExitOnError)
	port := set.Int("port", 0, "loopback port the model is served on (default: the model's assigned port)")
	set.Usage = ui.UsageFunc(set, "usage: fornax connect <model> [-port N]")
	var id string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		id, args = args[0], args[1:]
	}
	set.Parse(args)
	if id == "" {
		if set.NArg() == 1 {
			id = set.Arg(0)
		} else {
			return fmt.Errorf("usage: fornax connect <model> [-port N]")
		}
	} else if set.NArg() > 0 {
		return fmt.Errorf("usage: fornax connect <model> [-port N]")
	}
	spec := model(id)
	if spec == nil {
		return unknownModel(id)
	}
	if err := requireServer(spec); err != nil {
		return err
	}
	servePort := spec.Port
	if *port != 0 {
		servePort = *port
	}
	if spec.Runtime == catalog.Kev {
		if slices.Contains(servedModels(servePort, ""), kevAlias) {
			fmt.Println(kevBlock(paths.EndpointURL(servePort)))
			return nil
		}
		return fmt.Errorf("nothing is answering on 127.0.0.1:%d — is `%s` running?", servePort, spec.ID)
	}
	root := paths.Home()
	cfg, err := paths.LoadConfig(root)
	if err != nil {
		return err
	}
	if cfg.APIKey == "" {
		return fmt.Errorf("no server key yet — run `fornax run %s` once first", spec.ID)
	}
	if spec.Runtime == catalog.Laya {
		// Decision models have no Earshot route — print the SDK block.
		if slices.Contains(servedModels(servePort, cfg.APIKey), spec.ID) {
			fmt.Println(layaBlock(paths.EndpointURL(servePort), spec.ID, cfg.APIKey))
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
}
