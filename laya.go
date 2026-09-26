package main

// laya support — Convai Innovations' open-weights answer to kev: a
// non-autoregressive encoder (ModernBERT / mmBERT) that answers the same
// typed questions over the same /v1/systemone shape. The pypi package is a
// library with no server, so fornax embeds a stdlib shim (laya_serve.py)
// that loads a checkpoint dir and — unlike kev — requires the loopback key.
//
// fornax pins the laya source tarball and each checkpoint's files
// (safetensors, rl_agent_config.json, encoder and tokenizer configs) the
// same way it pins everything else, then builds a uv venv once.

import (
	"context"
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/earshot-run/fornax/internal/catalog"
	"github.com/earshot-run/fornax/internal/paths"
	"github.com/earshot-run/fornax/internal/ui"
)

// The pinned laya source — a GitHub commit archive, byte-counted and hashed
// like every other artifact.
var layaSource = catalog.Pin{
	File:     "laya-573e5b62.tar.gz",
	Revision: "573e5b62696ba441230cd6be71d593331b5d23af",
	Bytes:    1_866_233,
	SHA256:   "03931635a92b7609c6c253ac1d4d618ebe4fc54956744db01c027b504fd9c426",
}

const layaSourceURL = "https://github.com/NandhaKishorM/laya/archive/573e5b62696ba441230cd6be71d593331b5d23af.tar.gz"

//go:embed laya_serve.py
var layaServePy string

func layaRoot(root string) string      { return filepath.Join(root, "laya") }
func layaSrcDir(root string) string    { return filepath.Join(layaRoot(root), "src") }
func layaPython(root string) string    { return filepath.Join(layaSrcDir(root), ".venv", "bin", "python") }
func layaServePath(root string) string { return filepath.Join(layaRoot(root), "serve.py") }
func layaRuntimeReady(root string) bool {
	if _, err := os.Stat(layaPython(root)); err != nil {
		return false
	}
	data, err := os.ReadFile(filepath.Join(layaRoot(root), paths.Receipt))
	return err == nil && strings.TrimSpace(string(data)) == layaSource.SHA256
}

// The directory the shim should --model: the one holding
// rl_agent_config.json — the model dir itself for the English checkpoint,
// one level under it for the bundled subfolder checkpoints.
func layaModelDir(root string, spec *catalog.Spec) string {
	base := paths.ModelDir(root, spec)
	if _, err := os.Stat(filepath.Join(base, "rl_agent_config.json")); err == nil {
		return base
	}
	entries, err := os.ReadDir(base)
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		if entry.IsDir() {
			dir := filepath.Join(base, entry.Name())
			if _, err := os.Stat(filepath.Join(dir, "rl_agent_config.json")); err == nil {
				return dir
			}
		}
	}
	return ""
}

// Fetch the pinned source, unpack it, build the venv with uv and pip-install
// the package into it (the heavy part — torch and friends, a few GB on
// first run).
func ensureLayaRuntime(ctx context.Context, root string, progress func(int64)) error {
	if runtime.GOOS == "windows" {
		return fmt.Errorf("laya models need macOS or Linux (torch MPS/CUDA)")
	}
	if layaRuntimeReady(root) {
		return nil
	}
	uv, err := exec.LookPath("uv")
	if err != nil {
		return fmt.Errorf("laya needs `uv` to build its python env — install it from https://docs.astral.sh/uv/")
	}
	if err := paths.ProtectDir(layaRoot(root)); err != nil {
		return err
	}
	src, err := installSourceTree(ctx, layaRoot(root), layaSource, layaSourceURL, progress)
	if err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, ui.Dim("building laya python env with uv (torch download, one time)…"))
	venv := exec.CommandContext(ctx, uv, "venv", filepath.Join(src, ".venv"))
	venv.Dir = src
	venv.Env = inheritEnv()
	venv.Stdout = os.Stderr
	venv.Stderr = os.Stderr
	if err := venv.Run(); err != nil {
		return fmt.Errorf("uv venv failed: %w", err)
	}
	install := exec.CommandContext(ctx, uv, "pip", "install", "--python", layaPython(root), src)
	install.Dir = src
	install.Env = inheritEnv()
	install.Stdout = os.Stderr
	install.Stderr = os.Stderr
	if err := install.Run(); err != nil {
		return fmt.Errorf("uv pip install failed: %w", err)
	}
	return paths.AtomicPrivate(filepath.Join(layaRoot(root), paths.Receipt), []byte(layaSource.SHA256+"\n"))
}

func spawnLaya(root string, spec *catalog.Spec, port int, logPath string) (*exec.Cmd, error) {
	dir := layaModelDir(root, spec)
	if dir == "" {
		return nil, fmt.Errorf("%s has no installed checkpoint (missing rl_agent_config.json)", spec.ID)
	}
	// The shim ships inside the binary — refresh it every spawn so it always
	// matches this fornax build.
	if err := paths.AtomicPrivate(layaServePath(root), []byte(layaServePy)); err != nil {
		return nil, err
	}
	args := []string{
		layaServePath(root),
		"--model", dir,
		"--port", fmt.Sprintf("%d", port),
		"--alias", spec.ID,
		"--key-file", paths.KeyPath(root),
	}
	cmd := exec.Command(layaPython(root), args...)
	cmd.Dir = layaRoot(root)
	cmd.Env = inheritEnv()
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
		return nil, fmt.Errorf("could not start the laya server: %w", err)
	}
	return cmd, nil
}

// What a `run` prints for a laya model: it speaks TypeSafe's API behind the
// loopback key, not chat completions, so the Earshot connect block does not
// apply.
func layaBlock(url, model, key string) string {
	return fmt.Sprintf("    %s\n      %s %s\n      %s %s\n      %s %s",
		ui.Dim("TypeSafe SDK →"),
		ui.Dim("base_url:"), url,
		ui.Dim("model:"), model,
		ui.Dim("api_key:"), key)
}
