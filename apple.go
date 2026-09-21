package main

// Apple's Foundation Models runtime: the ~3B on-device model that ships with
// macOS 26+ on Apple Silicon. There are no weights to download — the model
// lives in the OS — so fornax compiles a tiny Swift bridge (bridge.swift,
// embedded below) that speaks JSONL over stdio, and fronts it with a loopback
// adapter that mimics llama-server's API: /health, /v1/models and
// /v1/chat/completions with SSE. Everything upstream — run, ask, chat, test,
// ps, connect — then works unchanged.

import (
	"bufio"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed bridge.swift
var appleBridgeSource []byte

const appleBridgeName = "fm-bridge"

func appleSupported() error {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		return fmt.Errorf("apple-fm is Apple's on-device model — it needs Apple Silicon on macOS 26+")
	}
	out, err := exec.Command("sw_vers", "-productVersion").Output()
	if err != nil {
		return fmt.Errorf("could not read the macOS version: %w", err)
	}
	major, _ := strconv.Atoi(strings.SplitN(strings.TrimSpace(string(out)), ".", 2)[0])
	if major < 26 {
		return fmt.Errorf("apple-fm needs macOS 26 or later (this is macOS %s)", strings.TrimSpace(string(out)))
	}
	return nil
}

func appleSourceSHA() string {
	sum := sha256.Sum256(appleBridgeSource)
	return hex.EncodeToString(sum[:])
}

func appleBridgePath(root string, spec *modelSpec) string {
	return filepath.Join(modelDir(root, spec), appleBridgeName)
}

// Installed = the compiled bridge exists, the source on disk matches the
// embedded copy, and the receipt recorded that source's digest.
func appleInstalled(root string, spec *modelSpec) bool {
	if info, err := os.Stat(appleBridgePath(root, spec)); err != nil || info.IsDir() {
		return false
	}
	data, err := os.ReadFile(filepath.Join(modelDir(root, spec), "bridge.swift"))
	if err != nil || fmt.Sprintf("%x", sha256.Sum256(data)) != appleSourceSHA() {
		return false
	}
	receiptData, err := os.ReadFile(filepath.Join(modelDir(root, spec), receipt))
	return err == nil && strings.TrimSpace(string(receiptData)) == appleSourceSHA()
}

// Compile the embedded Swift bridge once per fornax version (the receipt
// tracks the source digest, so a changed bridge.swift recompiles).
func ensureAppleModel(ctx context.Context, root string, spec *modelSpec) error {
	if err := appleSupported(); err != nil {
		return err
	}
	if appleInstalled(root, spec) {
		return nil
	}
	swiftc, err := exec.LookPath("swiftc")
	if err != nil {
		out, xerr := exec.Command("xcrun", "-f", "swiftc").Output()
		if xerr != nil {
			return fmt.Errorf("apple-fm needs a Swift compiler — install the Xcode Command Line Tools: xcode-select --install")
		}
		swiftc = strings.TrimSpace(string(out))
	}
	dir := modelDir(root, spec)
	if err := protectDir(dir); err != nil {
		return err
	}
	sourcePath := filepath.Join(dir, "bridge.swift")
	if err := atomicPrivate(sourcePath, appleBridgeSource); err != nil {
		return err
	}
	compiling := spin("compiling fm-bridge")
	tmp := filepath.Join(dir, appleBridgeName+".tmp-write")
	cmd := exec.CommandContext(ctx, swiftc, "-parse-as-library", "-O", "-o", tmp, "bridge.swift")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		compiling.stop("")
		os.Remove(tmp)
		return fmt.Errorf("swiftc failed: %w\n%s", err, strings.TrimSpace(string(out)))
	}
	if err := os.Chmod(tmp, 0o700); err != nil {
		compiling.stop("")
		return err
	}
	if err := os.Rename(tmp, appleBridgePath(root, spec)); err != nil {
		compiling.stop("")
		return fmt.Errorf("could not install the bridge: %w", err)
	}
	compiling.stop("")
	return atomicPrivate(filepath.Join(dir, receipt), []byte(appleSourceSHA()+"\n"))
}

// One JSONL line from the bridge.
type bridgeEvent struct {
	ID    int    `json:"id"`
	Ready bool   `json:"ready"`
	Token string `json:"token"`
	Reply string `json:"reply"`
	Done  bool   `json:"done"`
	Err   string `json:"error"`
}

// The compiled Swift process. One request at a time — the model is serial
// anyway, and the mutex keeps writes from interleaving.
type appleBridge struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	lines  *bufio.Reader
	exited chan error
	mu     sync.Mutex
	nextID int
}

func spawnAppleBridge(root string, spec *modelSpec, log *os.File) (*appleBridge, error) {
	cmd := exec.Command(appleBridgePath(root, spec))
	cmd.Dir = modelDir(root, spec)
	cmd.Env = []string{
		"HOME=" + filepath.Join(root, "server-home"),
		"PATH=/usr/bin:/bin:/usr/sbin:/sbin",
	}
	if tmp := os.Getenv("TMPDIR"); tmp != "" {
		cmd.Env = append(cmd.Env, "TMPDIR="+tmp)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if log != nil {
		cmd.Stderr = log
	} else {
		cmd.Stderr = os.Stderr
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("could not start fm-bridge: %w", err)
	}
	b := &appleBridge{
		cmd:    cmd,
		stdin:  stdin,
		lines:  bufio.NewReaderSize(stdout, 1<<20),
		exited: make(chan error, 1),
	}
	go func() { b.exited <- cmd.Wait() }()
	// First line is always the availability verdict.
	line, err := b.lines.ReadString('\n')
	if err != nil {
		b.kill()
		return nil, fmt.Errorf("fm-bridge died before it was ready: %w", err)
	}
	var ev bridgeEvent
	if json.Unmarshal([]byte(line), &ev) != nil {
		b.kill()
		return nil, fmt.Errorf("fm-bridge spoke garbage: %s", strings.TrimSpace(line))
	}
	if ev.Err != "" {
		b.kill()
		return nil, fmt.Errorf("apple-fm is not available: %s", ev.Err)
	}
	if !ev.Ready {
		b.kill()
		return nil, fmt.Errorf("fm-bridge did not come up")
	}
	return b, nil
}

func (b *appleBridge) alive() bool {
	select {
	case <-b.exited:
		return false
	default:
		return true
	}
}

func (b *appleBridge) kill() {
	b.stdin.Close()
	b.cmd.Process.Kill()
	select {
	case <-b.exited:
	case <-time.After(3 * time.Second):
	}
}

// One generation. messages are {role, content-string} maps — the adapter
// has already flattened or rejected non-text parts.
func (b *appleBridge) generate(messages []map[string]string, stream bool, onToken func(string)) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.alive() {
		return "", fmt.Errorf("fm-bridge is not running")
	}
	b.nextID++
	body, _ := json.Marshal(map[string]any{
		"id":       b.nextID,
		"messages": messages,
		"stream":   stream,
	})
	if _, err := b.stdin.Write(append(body, '\n')); err != nil {
		return "", fmt.Errorf("fm-bridge is not listening: %w", err)
	}
	var reply strings.Builder
	for {
		line, err := b.lines.ReadString('\n')
		if err != nil {
			return "", fmt.Errorf("fm-bridge stopped mid-reply: %w", err)
		}
		var ev bridgeEvent
		if json.Unmarshal([]byte(line), &ev) != nil {
			continue
		}
		if ev.ID != b.nextID {
			continue
		}
		if ev.Err != "" {
			return "", fmt.Errorf("%s", ev.Err)
		}
		if ev.Token != "" {
			reply.WriteString(ev.Token)
			if onToken != nil {
				onToken(ev.Token)
			}
		}
		if ev.Reply != "" {
			reply.WriteString(ev.Reply)
		}
		if ev.Done {
			return reply.String(), nil
		}
	}
}

// The loopback adapter: an http.Server that answers like llama-server.
type appleServer struct {
	root   string
	spec   *modelSpec
	key    string
	bridge *appleBridge
	srv    *http.Server
	ln     net.Listener
	log    *os.File
	mu     sync.Mutex // guards bridge respawn
}

func (s *appleServer) ensureBridge() (*appleBridge, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.bridge.alive() {
		return s.bridge, nil
	}
	fresh, err := spawnAppleBridge(s.root, s.spec, s.log)
	if err != nil {
		return nil, err
	}
	s.bridge = fresh
	return fresh, nil
}

// Serve an adapter on port; returns once it is listening (readiness is
// proven by waitReady polling /health + /v1/models, same as llama).
func startAppleServer(root string, spec *modelSpec, port int, key, logPath string) (*appleServer, error) {
	var log *os.File
	if logPath != "" {
		var err error
		log, err = os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, fmt.Errorf("could not open %s: %w", logPath, err)
		}
	}
	bridge, err := spawnAppleBridge(root, spec, log)
	if err != nil {
		if log != nil {
			log.Close()
		}
		return nil, err
	}
	s := &appleServer{root: root, spec: spec, key: key, bridge: bridge, log: log}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		bridge.kill()
		if log != nil {
			log.Close()
		}
		return nil, fmt.Errorf("port %d is already in use", port)
	}
	s.ln = ln
	s.srv = &http.Server{Handler: s.mux()}
	go s.srv.Serve(ln)
	return s, nil
}

func (s *appleServer) mux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.withAuth(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
	}))
	mux.HandleFunc("/v1/models", s.withAuth(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"object": "list",
			"data": []map[string]any{{
				"id": s.spec.id, "object": "model", "created": 0, "owned_by": "apple",
			}},
		})
	}))
	mux.HandleFunc("/v1/chat/completions", s.withAuth(s.chatCompletions))
	return mux
}

func (s *appleServer) withAuth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+s.key {
			writeJSON(w, http.StatusUnauthorized, map[string]any{
				"error": map[string]any{"message": "missing or wrong API key"},
			})
			return
		}
		h(w, r)
	}
}

// Pull the plain-text content out of an OpenAI message; anything else
// (image/audio parts) is a hard no for a text-only model.
func appleText(content any) (string, error) {
	switch v := content.(type) {
	case string:
		return v, nil
	case []any:
		var text strings.Builder
		for _, part := range v {
			obj, ok := part.(map[string]any)
			if !ok {
				return "", fmt.Errorf("unreadable content part")
			}
			if obj["type"] == "text" {
				if t, ok := obj["text"].(string); ok {
					text.WriteString(t)
				}
				continue
			}
			return "", fmt.Errorf("apple-fm is text-only — it cannot take %s parts", obj["type"])
		}
		return text.String(), nil
	}
	return "", fmt.Errorf("unreadable message content")
}

func (s *appleServer) chatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": map[string]any{"message": "POST only"}})
		return
	}
	var req struct {
		Messages []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
		Stream bool `json:"stream"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, maxHTTPBody)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "unreadable request: " + err.Error()}})
		return
	}
	messages := make([]map[string]string, 0, len(req.Messages))
	for _, m := range req.Messages {
		text, err := appleText(m.Content)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": err.Error()}})
			return
		}
		role := m.Role
		if role != "system" && role != "assistant" {
			role = "user"
		}
		messages = append(messages, map[string]string{"role": role, "content": text})
	}
	if len(messages) == 0 || messages[len(messages)-1]["role"] != "user" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "the last message must be from the user"}})
		return
	}
	bridge, err := s.ensureBridge()
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": map[string]any{"message": err.Error()}})
		return
	}
	started := time.Now()
	if !req.Stream {
		reply, err := bridge.generate(messages, false, nil)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": map[string]any{"message": err.Error()}})
			return
		}
		elapsed := time.Since(started).Seconds()
		writeJSON(w, http.StatusOK, map[string]any{
			"id":     "chatcmpl-apple",
			"object": "chat.completion",
			"model":  s.spec.id,
			"choices": []map[string]any{{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": reply},
				"finish_reason": "stop",
			}},
			// The framework reports no token counts — ~4 chars/token is the
			// standard rough estimate, and it feeds `test`'s tok/s line.
			"usage": map[string]any{
				"prompt_tokens":     appleTokens(messages),
				"completion_tokens": max(1, len(reply)/4),
			},
			"timings": map[string]any{"predicted_per_second": float64(max(1, len(reply)/4)) / max(elapsed, 0.001)},
		})
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": map[string]any{"message": "streaming unsupported"}})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	chunk := func(delta string) {
		data, _ := json.Marshal(map[string]any{
			"id":     "chatcmpl-apple",
			"object": "chat.completion.chunk",
			"model":  s.spec.id,
			"choices": []map[string]any{{
				"index": 0,
				"delta": map[string]any{"content": delta},
			}},
		})
		fmt.Fprintf(w, "data: %s\n\n", data)
		flusher.Flush()
	}
	_, err = bridge.generate(messages, true, chunk)
	if err != nil {
		data, _ := json.Marshal(map[string]any{"error": map[string]any{"message": err.Error()}})
		fmt.Fprintf(w, "data: %s\n\n", data)
		flusher.Flush()
		return
	}
	fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()
}

// Rough prompt-token estimate across the request messages.
func appleTokens(messages []map[string]string) int {
	total := 0
	for _, m := range messages {
		total += len(m["content"])
	}
	return total / 4
}

func (s *appleServer) shutdown() {
	s.srv.Close()
	s.bridge.kill()
	if s.log != nil {
		s.log.Close()
	}
}

// The apple path through withServer: reuse a running adapter, else compile
// the bridge if needed, serve in-process on a scratch port, tear down after.
func withApple(ctx context.Context, root string, spec *modelSpec, fn func(url, key string) error) error {
	key, err := ensureKey(root)
	if err != nil {
		return err
	}
	if contains(servedModels(spec.port, key), spec.id) {
		fmt.Fprintf(os.Stderr, "%s\n", dim(fmt.Sprintf("reusing %s on :%d", spec.id, spec.port)))
		return fn(endpointURL(spec.port), key)
	}
	port, err := freePort(scratchPortBase)
	if err != nil {
		return err
	}
	loading := spin("loading " + spec.id)
	logPath := filepath.Join(root, "server.log")
	srv, err := startAppleServer(root, spec, port, key, logPath)
	if err != nil {
		loading.stop("")
		return fmt.Errorf("%w — server log: %s", err, logPath)
	}
	defer srv.shutdown()
	loading.stop("")
	return fn(endpointURL(port), key)
}

// `run apple-fm`: the adapter serves in the foreground until Ctrl-C, same
// shape as the llama path in cmdRun — waitReady, panel, then babysit the
// bridge process.
func runApple(ctx context.Context, root string, spec *modelSpec, port int, ctxIgnored, noConnect bool) error {
	if ctxIgnored {
		fmt.Fprintln(os.Stderr, "note: -ctx-size is ignored for apple-fm (the OS manages context)")
	}
	key, err := ensureKey(root)
	if err != nil {
		return err
	}
	if err := portFree(port); err != nil {
		return err
	}
	loading := spin("loading " + spec.id)
	srv, err := startAppleServer(root, spec, port, key, "")
	if err != nil {
		loading.stop("")
		return err
	}
	if err := waitReady(ctx, srv.bridge.exited, port, spec.id, key, true); err != nil {
		loading.stop("")
		srv.shutdown()
		return err
	}
	loading.stop("")
	url := endpointURL(port)
	fmt.Printf("\n%s %s\n", markOK(), bold(spec.name)+" is serving")
	fmt.Printf("    %s %s\n", dim("url:"), cyan(url))
	if noConnect {
		fmt.Println(pasteBlock(url, key, spec.id))
	} else {
		reportConnect(url, key, spec.id)
	}
	fmt.Println(dim("\nctrl-c to stop"))
	select {
	case <-ctx.Done():
		fmt.Fprintln(os.Stderr, dim("stopping…"))
		srv.shutdown()
		return nil
	case status := <-srv.bridge.exited:
		srv.shutdown()
		return fmt.Errorf("fm-bridge exited: %v", status)
	}
}

// A latency sample like runKevBench: a few timed non-stream calls.
func runAppleBench(ctx context.Context, spec *modelSpec, calls int) error {
	return withServer(ctx, spec, nil, func(url, key string) error {
		var lat []float64
		for i := 0; i < calls; i++ {
			started := time.Now()
			if _, err := chatOnce(ctx, url, key, spec.id,
				[]message{textMessage("user", "Reply with exactly: ok")}, 8); err != nil {
				return err
			}
			ms := float64(time.Since(started).Milliseconds())
			lat = append(lat, ms)
			fmt.Printf("  %s %.0f ms\n", dim(fmt.Sprintf("run %d", i+1)), ms)
		}
		sort.Float64s(lat)
		fmt.Printf("%s %s — median %.0f ms over %d requests\n", green("✓"), bold(spec.id), lat[len(lat)/2], len(lat))
		return nil
	})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(value)
}
