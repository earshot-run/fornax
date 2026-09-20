package main

// Pulling weights and running `llama-server` in the foreground: spawn with a
// scrubbed environment, prove `/health` and `/v1/models`, then babysit until
// Ctrl-C or the child exits.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"
)

const (
	loadTimeout = 15 * time.Minute
	readyPoll   = 500 * time.Millisecond
	maxHTTPBody = 2 * 1024 * 1024
)

// Download every artifact a model needs, verify each, promote into place.
// Resumable: a `.part` keeps its bytes between runs.
func ensureModel(ctx context.Context, root string, spec *modelSpec, progress func(int64)) error {
	for _, pin := range spec.files() {
		installed := fileInstalled(root, spec, pin)
		if installed {
			progress(pin.bytes)
			continue
		}
		part, err := prepareCandidate(root, spec, pin)
		if err != nil {
			return err
		}
		if err := fetch(ctx, spec.url(pin), pin.bytes, part, progress); err != nil {
			return err
		}
		if err := verify(part, pin.bytes, pin.sha256); err != nil {
			return err
		}
		if err := promote(root, spec, pin, part); err != nil {
			return err
		}
	}
	return writeReceipt(modelDir(root, spec), spec)
}

func fileInstalled(root string, spec *modelSpec, pin *filePin) bool {
	info, err := os.Stat(filePath(root, spec, pin))
	return err == nil && info.Size() == pin.bytes
}

func prepareCandidate(root string, spec *modelSpec, pin *filePin) (string, error) {
	dir := modelDir(root, spec)
	if err := protectDir(dir); err != nil {
		return "", err
	}
	part := partPath(root, spec, pin)
	final := filePath(root, spec, pin)
	// A verified-but-unpromoted file from an earlier crash resumes as a part.
	if _, err := os.Stat(part); os.IsNotExist(err) {
		if info, err := os.Stat(final); err == nil && info.Size() == pin.bytes {
			if err := os.Rename(final, part); err != nil {
				return "", fmt.Errorf("could not resume model verification: %w", err)
			}
		}
	}
	if info, err := os.Stat(part); err == nil && info.Size() > pin.bytes {
		os.Remove(part)
	}
	return part, nil
}

func promote(root string, spec *modelSpec, pin *filePin, part string) error {
	final := filePath(root, spec, pin)
	os.Remove(final)
	if err := os.Rename(part, final); err != nil {
		return fmt.Errorf("could not install model: %w", err)
	}
	return nil
}

// Re-hash every pinned file before each spawn: the install receipt cannot
// authorize bytes that may have changed since.
func rehash(root string, spec *modelSpec) error {
	for _, pin := range spec.files() {
		if err := verify(filePath(root, spec, pin), pin.bytes, pin.sha256); err != nil {
			return err
		}
	}
	return nil
}

// logPath "" inherits the terminal (the `run` case); a path redirects the
// server's chatter to a file so one-shot commands keep stdout clean.
func spawnServer(root string, eng *engineSpec, spec *modelSpec, port int, ctxSize int, logPath string) (*exec.Cmd, error) {
	binary := engineBinary(root, eng, eng.binary)
	args := []string{
		"--model", modelFinal(root, spec),
		"--alias", spec.id,
		"--host", "127.0.0.1",
		"--port", fmt.Sprintf("%d", port),
		"--ctx-size", fmt.Sprintf("%d", ctxSize),
		"--parallel", "1",
		"--jinja",
		"--api-key-file", keyPath(root),
		"--no-ui",
	}
	if spec.mmproj != nil {
		args = append(args, "--mmproj", filePath(root, spec, spec.mmproj))
	}
	if spec.kind == modalVision {
		// llama.cpp warns below 1024 on Qwen-VL grounding tasks.
		args = append(args, "--image-min-tokens", "1024")
	}
	if spec.kind == modalText {
		// Qwen3 emits thinking traces unless reasoning is off.
		args = append(args, "--reasoning", "off")
	}
	if runtime.GOOS != "windows" {
		// The layer count is ignored where there is no offload backend.
		args = append(args, "--n-gpu-layers", "999")
	}
	cmd := exec.Command(binary, args...)
	cmd.Dir = filepath.Dir(binary)
	// Never inherit LLAMA_ARG_*, provider credentials, preload variables,
	// or DYLD injection from the caller.
	cmd.Env = []string{
		"HOME=" + filepath.Join(root, "server-home"),
		"PATH=/usr/bin:/bin:/usr/sbin:/sbin",
	}
	if tmp := os.Getenv("TMPDIR"); tmp != "" {
		cmd.Env = append(cmd.Env, "TMPDIR="+tmp)
	}
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
func waitReady(ctx context.Context, exited <-chan error, port int, alias, key string) error {
	client := &http.Client{
		Timeout:   3 * time.Second,
		Transport: &http.Transport{Proxy: nil},
	}
	started := time.Now()
	root := fmt.Sprintf("http://127.0.0.1:%d", port)
	for {
		select {
		case status := <-exited:
			return fmt.Errorf("llama-server exited before it was ready (%v)", status)
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if time.Since(started) >= loadTimeout {
			return fmt.Errorf("the model did not load within %d minutes", int(loadTimeout.Minutes()))
		}
		health := authenticatedGet(client, root+"/health", key)
		listed := false
		if models := authenticatedGet(client, root+"/v1/models", key); models != nil {
			if data, ok := models["data"].([]any); ok {
				for _, row := range data {
					if obj, ok := row.(map[string]any); ok && obj["id"] == alias {
						listed = true
					}
				}
			}
		}
		if health != nil && health["status"] == "ok" && listed {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case status := <-exited:
			return fmt.Errorf("llama-server exited before it was ready (%v)", status)
		case <-time.After(readyPoll):
		}
	}
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
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxHTTPBody))
	if err != nil {
		return nil
	}
	var value map[string]any
	if json.Unmarshal(body, &value) != nil {
		return nil
	}
	return value
}

// What the running server advertises, or nil when it is not up (or the key
// is not its key).
func servedModels(port int, key string) []string {
	client := &http.Client{
		Timeout:   3 * time.Second,
		Transport: &http.Transport{Proxy: nil},
	}
	models := authenticatedGet(client, fmt.Sprintf("http://127.0.0.1:%d/v1/models", port), key)
	if models == nil {
		return nil
	}
	data, ok := models["data"].([]any)
	if !ok {
		return nil
	}
	var ids []string
	for _, row := range data {
		if obj, ok := row.(map[string]any); ok {
			if id, ok := obj["id"].(string); ok {
				ids = append(ids, id)
			}
		}
	}
	return ids
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
