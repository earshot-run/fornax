package modelrt

// kev support — a different kind of local model. Kev is jaredpalmer's
// Jev-style decision model: a LoRA adapter plus readout head on a Qwen base,
// served by a small FastAPI app (`python -m kev.serve`). It answers typed
// questions with calibrated probabilities over TypeSafe's /v1/systemone API —
// it does not chat, so it cannot run under llama-server.
//
// fornax installs kev from its repo's HEAD and each checkpoint tarball from
// its release, then bootstraps a uv venv once.
// The judge command and the /v1/systemone client it speaks live in
// fornax's root judge.go — laya answers the same protocol.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/earshot-run/fornax/internal/catalog"
	"github.com/earshot-run/fornax/internal/download"
	"github.com/earshot-run/fornax/internal/engine"
	"github.com/earshot-run/fornax/internal/paths"
	"github.com/earshot-run/fornax/internal/ui"
)

const (
	kevRepo  = "jaredpalmer/kev"
	kevAlias = "kev-latest"
)

func kevRoot(root string) string       { return filepath.Join(root, "kev") }
func kevSrcDir(root string) string     { return filepath.Join(kevRoot(root), "src") }
func kevPython(root string) string     { return filepath.Join(kevSrcDir(root), ".venv", "bin", "python") }
func KevRuntimeReady(root string) bool { return sourceReady(kevRoot(root), kevPython(root)) }

// The directory kev.serve should --run: the unpacked checkpoint dir holding
// head.pt, one level under models/<id>/.
func KevCkptDir(root string, spec *catalog.Spec) string {
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

// Shared by the python runtimes (kev, laya): download repo's HEAD into
// <home> and promote the archive's single top-level directory to
// <home>/src. Returns the src dir.
func installSourceTree(ctx context.Context, home, repo string) (string, error) {
	archive := filepath.Join(home, "src.tar.gz.part")
	defer os.Remove(archive)
	fetching := ui.Spin("downloading " + repo)
	// GitHub streams commit archives without a length.
	err := download.Fetch(ctx, "https://github.com/"+repo+"/archive/HEAD.tar.gz", 0, archive, func(int64) {})
	fetching.Stop("")
	if err != nil {
		return "", err
	}
	src := filepath.Join(home, "src")
	staging := src + ".staging"
	os.RemoveAll(staging)
	defer os.RemoveAll(staging)
	if err := paths.ProtectDir(staging); err != nil {
		return "", err
	}
	if err := engine.UnpackTarGz(archive, staging); err != nil {
		return "", err
	}
	// The archive wraps everything in <name>-<sha>/ — promote that dir.
	entries, err := os.ReadDir(staging)
	if err != nil || len(entries) != 1 || !entries[0].IsDir() {
		return "", fmt.Errorf("the source archive has an unexpected layout")
	}
	os.RemoveAll(src)
	if err := os.Rename(filepath.Join(staging, entries[0].Name()), src); err != nil {
		return "", fmt.Errorf("could not install the source tree: %w", err)
	}
	return src, nil
}

// A python runtime is ready once its venv exists and its receipt, written
// last, says the build finished.
func sourceReady(home, python string) bool {
	if _, err := os.Stat(python); err != nil {
		return false
	}
	_, err := os.Stat(filepath.Join(home, paths.Receipt))
	return err == nil
}

// Fetch the source, unpack it, build the venv with uv (the heavy part —
// torch and friends, a few GB on first run).
func ensureKevRuntime(ctx context.Context, root string) error {
	if runtime.GOOS == "windows" {
		return fmt.Errorf("kev models need macOS or Linux (torch MPS/CUDA)")
	}
	if KevRuntimeReady(root) {
		return nil
	}
	uv, err := exec.LookPath("uv")
	if err != nil {
		return fmt.Errorf("kev needs `uv` to build its python env — install it from https://docs.astral.sh/uv/")
	}
	if err := paths.ProtectDir(kevRoot(root)); err != nil {
		return err
	}
	src, err := installSourceTree(ctx, kevRoot(root), kevRepo)
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
	return paths.AtomicPrivate(filepath.Join(kevRoot(root), paths.Receipt), []byte(kevRepo+"@HEAD\n"))
}

// kev and laya run on the user's real environment — kev downloads its Qwen
// base (at the revision the checkpoint names) from Hugging Face into ~/.cache/huggingface on first load.
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

func spawnKev(root string, spec *catalog.Spec, port int, key, logPath string) (*exec.Cmd, error) {
	ckpt := KevCkptDir(root, spec)
	if ckpt == "" {
		return nil, fmt.Errorf("%s has no unpacked checkpoint (missing head.pt)", spec.ID)
	}
	python := kevPython(root)
	args := []string{"-m", "kev.serve", "--run", ckpt, "--port", fmt.Sprintf("%d", port)}
	cmd := exec.Command(python, args...)
	cmd.Dir = kevSrcDir(root)
	// kev.serve turns on its fused Qwen3.5 kernels on CUDA, but they import
	// flash-linear-attention, which no kev extra installs (checked at 1b62aa2d):
	// on CUDA the default dies with "No module named 'fla'". The plain torch
	// path gives the same answers.
	cmd.Env = append(inheritEnv(), "KEV_API_KEY="+key, "KEV_FUSED=0")
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

// The model name a decision server expects in the request: kev answers
// under its family alias, laya under the id it was spawned as.
func DecisionModel(spec *catalog.Spec) string {
	if spec.Runtime == catalog.Kev {
		return kevAlias
	}
	return spec.ID
}
