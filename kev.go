package main

// kev support — a different kind of local model. Kev is jaredpalmer's
// Jev-style decision model: a LoRA adapter plus readout head on a Qwen3 base,
// served by a small FastAPI app (`python -m kev.serve`). It answers typed
// questions with calibrated probabilities over TypeSafe's /v1/systemone API —
// it does not chat, so it cannot run under llama-server.
//
// fornax pins the kev source tarball and each checkpoint tarball the
// same way it pins llama.cpp and GGUFs, then bootstraps a uv venv once.
// The judge command and the /v1/systemone client it speaks live in
// judge.go — laya answers the same protocol.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/earshot-run/fornax/internal/catalog"
	"github.com/earshot-run/fornax/internal/download"
	"github.com/earshot-run/fornax/internal/engine"
	"github.com/earshot-run/fornax/internal/paths"
	"github.com/earshot-run/fornax/internal/ui"
)

// The pinned kev source — a GitHub commit archive, byte-counted and hashed
// like every other artifact.
var kevSource = catalog.Pin{
	File:     "kev-90990a5f.tar.gz",
	Revision: "90990a5fac2995b9faa3190f7d437e84f2067768",
	Bytes:    20_314_594,
	SHA256:   "8d438f17b446a32f43002942b2a90314f77c6cd551fc6f84d182b8be3d5a1e00",
}

const kevSourceURL = "https://github.com/jaredpalmer/kev/archive/90990a5fac2995b9faa3190f7d437e84f2067768.tar.gz"
const kevAlias = "kev-latest"

func kevRoot(root string) string   { return filepath.Join(root, "kev") }
func kevSrcDir(root string) string { return filepath.Join(kevRoot(root), "src") }
func kevPython(root string) string { return filepath.Join(kevSrcDir(root), ".venv", "bin", "python") }
func kevRuntimeReady(root string) bool {
	if _, err := os.Stat(kevPython(root)); err != nil {
		return false
	}
	data, err := os.ReadFile(filepath.Join(kevRoot(root), paths.Receipt))
	return err == nil && strings.TrimSpace(string(data)) == kevSource.SHA256
}

// The directory kev.serve should --run: the unpacked checkpoint dir holding
// head.pt, one level under models/<id>/.
func kevCkptDir(root string, spec *catalog.Spec) string {
	base := paths.ModelDir(root, spec)
	if _, err := os.Stat(filepath.Join(base, "head.pt")); err == nil {
		return base
	}
	entries, err := os.ReadDir(base)
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		if entry.IsDir() {
			dir := filepath.Join(base, entry.Name())
			if _, err := os.Stat(filepath.Join(dir, "head.pt")); err == nil {
				return dir
			}
		}
	}
	return ""
}

// Shared by the python runtimes (kev, laya): fetch the pinned source
// tarball into <home>, verify it, and promote the archive's single
// top-level directory to <home>/src. Returns the src dir.
func installSourceTree(ctx context.Context, home string, pin catalog.Pin, url string, progress func(int64)) (string, error) {
	part := filepath.Join(home, pin.File+".part")
	final := filepath.Join(home, pin.File)
	// A tarball left from a previous run still has to match the pin.
	if _, err := os.Stat(final); err == nil {
		if err := download.Verify(final, pin.Bytes, pin.SHA256); err != nil {
			os.Remove(final)
		}
	}
	if _, err := os.Stat(final); os.IsNotExist(err) {
		if info, err := os.Stat(part); err == nil && info.Size() > pin.Bytes {
			os.Remove(part)
		}
		if err := download.Fetch(ctx, url, pin.Bytes, part, progress); err != nil {
			return "", err
		}
		if err := download.Verify(part, pin.Bytes, pin.SHA256); err != nil {
			return "", err
		}
		if err := os.Rename(part, final); err != nil {
			return "", fmt.Errorf("could not install the source archive: %w", err)
		}
	}
	src := filepath.Join(home, "src")
	staging := src + ".staging"
	os.RemoveAll(staging)
	defer os.RemoveAll(staging)
	if err := paths.ProtectDir(staging); err != nil {
		return "", err
	}
	if err := engine.UnpackTarGz(final, staging); err != nil {
		return "", err
	}
	// The archive wraps everything in <name>-<sha>/ — promote that dir.
	entries, err := os.ReadDir(staging)
	if err != nil || len(entries) != 1 || !entries[0].IsDir() {
		os.RemoveAll(staging)
		return "", fmt.Errorf("the source archive has an unexpected layout")
	}
	os.RemoveAll(src)
	if err := os.Rename(filepath.Join(staging, entries[0].Name()), src); err != nil {
		os.RemoveAll(staging)
		return "", fmt.Errorf("could not install the source tree: %w", err)
	}
	os.RemoveAll(staging)
	return src, nil
}

// Fetch the pinned source, unpack it, build the venv with uv (the heavy part —
// torch and friends, a few GB on first run).
func ensureKevRuntime(ctx context.Context, root string, progress func(int64)) error {
	if runtime.GOOS == "windows" {
		return fmt.Errorf("kev models need macOS or Linux (torch MPS/CUDA)")
	}
	if kevRuntimeReady(root) {
		return nil
	}
	uv, err := exec.LookPath("uv")
	if err != nil {
		return fmt.Errorf("kev needs `uv` to build its python env — install it from https://docs.astral.sh/uv/")
	}
	if err := paths.ProtectDir(kevRoot(root)); err != nil {
		return err
	}
	src, err := installSourceTree(ctx, kevRoot(root), kevSource, kevSourceURL, progress)
	if err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, ui.Dim("building kev python env with uv (torch download, one time)…"))
	sync := exec.CommandContext(ctx, uv, "sync", "--extra", "serve")
	sync.Dir = src
	sync.Env = inheritEnv()
	sync.Stdout = os.Stderr
	sync.Stderr = os.Stderr
	if err := sync.Run(); err != nil {
		return fmt.Errorf("uv sync failed: %w", err)
	}
	return paths.AtomicPrivate(filepath.Join(kevRoot(root), paths.Receipt), []byte(kevSource.SHA256+"\n"))
}

// kev and laya run on the user's real environment — kev downloads the Qwen3
// base from Hugging Face into ~/.cache/huggingface on first load.
func inheritEnv() []string {
	home, _ := os.UserHomeDir()
	env := []string{
		"HOME=" + home,
		"PATH=" + os.Getenv("PATH"),
		"PYTHONUNBUFFERED=1",
	}
	if tmp := os.Getenv("TMPDIR"); tmp != "" {
		env = append(env, "TMPDIR="+tmp)
	}
	for _, key := range []string{
		"HF_HOME", "HF_TOKEN", "UV_PYTHON", "UV_CACHE_DIR",
		"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY",
		"http_proxy", "https_proxy", "all_proxy", "no_proxy",
	} {
		if v := os.Getenv(key); v != "" {
			env = append(env, key+"="+v)
		}
	}
	return env
}

func spawnKev(root string, spec *catalog.Spec, port int, logPath string) (*exec.Cmd, error) {
	ckpt := kevCkptDir(root, spec)
	if ckpt == "" {
		return nil, fmt.Errorf("%s has no unpacked checkpoint (missing head.pt)", spec.ID)
	}
	python := kevPython(root)
	args := []string{"-m", "kev.serve", "--run", ckpt, "--port", fmt.Sprintf("%d", port)}
	cmd := exec.Command(python, args...)
	cmd.Dir = kevSrcDir(root)
	cmd.Env = inheritEnv()
	if spec.DType != "" {
		cmd.Env = append(cmd.Env, "KEV_DTYPE="+spec.DType)
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
		return nil, fmt.Errorf("could not start kev.serve: %w", err)
	}
	return cmd, nil
}

// What a `run` prints for a kev model: it speaks TypeSafe's API, not chat
// completions, so the Earshot connect block does not apply.
func kevBlock(url string) string {
	return fmt.Sprintf("    %s\n      %s %s\n      %s %s\n      %s \"local\"  %s",
		ui.Dim("TypeSafe SDK →"),
		ui.Dim("base_url:"), url,
		ui.Dim("model:"), kevAlias,
		ui.Dim("api_key:"), ui.Dim("(kev has no auth — loopback only)"))
}
