package modelrt

// stable-diffusion.cpp — image and video generation through an sd-cli
// build from upstream's newest release. sd-cli is a foreground process, not
// a server: prompt in, file out, exit. It unpacks beside the llama engine
// under engine/sd-<backend> so the two never share a dir or a receipt.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"time"

	"github.com/earshot-run/fornax/internal/catalog"
	"github.com/earshot-run/fornax/internal/paths"
	"github.com/earshot-run/fornax/internal/ui"
)

// Every stable-diffusion.cpp build fornax runs, by GOOS/GOARCH, the plain
// one first. Upstream ships no darwin/amd64, linux/arm64 or windows/arm64
// builds, so those platforms get no engine. Its Linux CUDA build ships only
// inside its container image, without the libraries the image's base layers
// provide: llama.cpp's CUDA 12 runtime asset carries cudart and cuBLAS, and
// NVIDIA's NCCL wheel the libnccl.so.2 it links against.
var sdEngines = func() map[string][]*catalog.EngineSpec {
	const repo = "leejet/stable-diffusion.cpp"
	build := func(asset string, backend catalog.Backend, binary string) *catalog.EngineSpec {
		return &catalog.EngineSpec{
			Name:    "stable-diffusion.cpp",
			Repo:    repo,
			Asset:   `^sd-[^-]+-[0-9a-f]+-bin-` + asset + `$`,
			DirName: "sd-" + string(backend),
			Backend: backend,
			Binary:  binary,
		}
	}
	// Both built on Ubuntu 24.04: glibc 2.38 or newer.
	linux := func(eng *catalog.EngineSpec) *catalog.EngineSpec {
		eng.MinGlibc = "2.38"
		return eng
	}
	cuda := build("", catalog.CUDA, "sd-cli.exe")
	cuda.Asset = `^sd-[^-]+-[0-9a-f]+-bin-win-cuda12-x64\.zip$`
	cuda.MinDriver = 551
	cuda.Parts = []catalog.EnginePart{{Repo: repo, Asset: `^cudart-sd-bin-win-cu12-x64\.zip$`}}
	linuxCUDA := linux(&catalog.EngineSpec{
		Name:       "stable-diffusion.cpp",
		Image:      "ghcr.io/leejet/stable-diffusion.cpp:master-cuda",
		ImageLayer: "/sd.cpp/bin",
		DirName:    "sd-cuda",
		Backend:    catalog.CUDA,
		// llama.cpp's CUDA 12 runtime is 12.8, which wants driver 570.
		MinDriver: 570,
		Parts: []catalog.EnginePart{
			{Repo: "ggml-org/llama.cpp", Asset: `^cudart-llama-[^-]+-bin-ubuntu-cuda-12\.\d+-x64\.tar\.gz$`},
			{PyPI: "nvidia-nccl-cu12", Asset: `manylinux.*_x86_64\.whl$`, Dir: "nvidia/nccl/lib"},
		},
		Binary: "sd-cli",
	})
	return map[string][]*catalog.EngineSpec{
		"darwin/arm64": {build(`Darwin-macOS-[\d.]+-arm64\.zip`, catalog.Metal, "sd-cli")},
		"linux/amd64": {
			linux(build(`Linux-Ubuntu-[\d.]+-x86_64\.zip`, catalog.CPU, "sd-cli")),
			linuxCUDA,
			linux(build(`Linux-Ubuntu-[\d.]+-x86_64-vulkan\.zip`, catalog.Vulkan, "sd-cli")),
		},
		"windows/amd64": {
			build(`win-cpu-x64\.zip`, catalog.CPU, "sd-cli.exe"),
			cuda,
			build(`win-vulkan-x64\.zip`, catalog.Vulkan, "sd-cli.exe"),
		},
	}
}()

// This machine's stable-diffusion.cpp build; an error means none fits.
func SDEngine() (*catalog.EngineSpec, error) {
	eng, err := pickEngine(sdEngines[runtime.GOOS+"/"+runtime.GOARCH], ProbeGPU(), os.Getenv("FORNAX_BACKEND"))
	if err != nil {
		return nil, err
	}
	if eng == nil {
		return nil, fmt.Errorf("stable-diffusion.cpp publishes no build for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	return eng, nil
}

// Pull() only knows the llama engine; the same steps run here against the
// sd engine dir.
func PrepareSD(ctx context.Context, spec *catalog.Spec) (string, *catalog.EngineSpec, error) {
	eng, err := SDEngine()
	if err != nil {
		return "", nil, err
	}
	root := paths.Home()
	if err := paths.ProtectDir(root); err != nil {
		return "", nil, err
	}
	if err := ensureEngine(ctx, root, eng, "sd engine"); err != nil {
		return "", nil, err
	}
	if Installed(root, spec) {
		fmt.Fprintf(os.Stderr, "%s\n", ui.Dim(spec.ID+" already installed"))
		return root, eng, nil
	}
	bar := ui.NewProgress(spec.ID, spec.TotalBytes())
	if err := ensureModel(ctx, root, spec, bar.Set); err != nil {
		return "", nil, err
	}
	fmt.Fprintf(os.Stderr, "%s %s installed (%s)\n", ui.Green("✓"), ui.Bold(spec.ID), ui.Dim(ui.HumanSize(spec.TotalBytes())))
	return root, eng, nil
}

// A full checkpoint loads with -m; weights that arrive with companion files
// are a standalone diffusion model.
func sdArgs(root string, spec *catalog.Spec, prompt, outPath string, seed int64, extra []string) []string {
	modelFlag := "-m"
	if len(spec.Companions) > 0 {
		modelFlag = "--diffusion-model"
	}
	args := []string{modelFlag, paths.ModelFinal(root, spec)}
	if spec.Kind == catalog.Video {
		args = append(args, "-M", "vid_gen")
	}
	for i := range spec.Companions {
		args = append(args, "--"+spec.Companions[i].Flag, paths.FilePath(root, spec, &spec.Companions[i]))
	}
	args = append(args, "-p", prompt, "-o", outPath, "--seed", strconv.FormatInt(seed, 10))
	args = append(args, spec.Args...)
	return append(args, extra...)
}

func RunSD(ctx context.Context, root string, eng *catalog.EngineSpec, spec *catalog.Spec, prompt, outPath string, seed int64, extra []string) error {
	cmd, outPath, err := SDCommand(ctx, root, eng, spec, prompt, outPath, seed, extra)
	if err != nil {
		return err
	}
	// sd-cli narrates every step; keep stdout to just the result line.
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	started := time.Now()
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("sd-cli exited: %w", err)
	}
	info, err := os.Stat(outPath)
	if err != nil || info.Size() == 0 {
		return fmt.Errorf("sd-cli finished but did not write %s", outPath)
	}
	fmt.Printf("%s wrote %s (%.0fs)\n", ui.Green("✓"), outPath, time.Since(started).Seconds())
	return nil
}

// The sd-cli invocation for one generation, and the absolute path it will
// write: cmd.Dir is the engine dir, so a relative path would land there.
func SDCommand(ctx context.Context, root string, eng *catalog.EngineSpec, spec *catalog.Spec, prompt, outPath string, seed int64, extra []string) (*exec.Cmd, string, error) {
	outPath, err := filepath.Abs(outPath)
	if err != nil {
		return nil, "", err
	}
	binary := paths.EngineBinary(root, eng, eng.Binary)
	cmd := exec.CommandContext(ctx, binary, sdArgs(root, spec, prompt, outPath, seed, extra)...)
	// sd-cli loads its shared libs from beside the binary.
	cmd.Dir = filepath.Dir(binary)
	cmd.Env = engineEnv(root, binary)
	cmd.Stdin = nil
	return cmd, outPath, nil
}
