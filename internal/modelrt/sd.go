package modelrt

// stable-diffusion.cpp — image and video generation through a pinned sd-cli
// build, the same pinned-engine + pinned-weights pattern as the llama.cpp
// path. sd-cli is a foreground process, not a server: prompt in, file out,
// exit. It unpacks beside the llama engine under engine/<tag> so the two
// never share a dir or a receipt.

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
	"github.com/earshot-run/fornax/internal/engine"
	"github.com/earshot-run/fornax/internal/paths"
	"github.com/earshot-run/fornax/internal/ui"
)

// The pinned stable-diffusion.cpp release (github.com/leejet), every build
// fornax runs, the plain one first. Sizes and digests are GitHub's asset
// digests; upstream ships no darwin/amd64, linux/arm64 or windows/arm64
// assets, so those platforms get no engine, and no Linux CUDA build, so an
// NVIDIA card on Linux runs it through Vulkan.
const sdVersion = "sd-master-c678dfe"

var sdEngines = func() map[string][]*catalog.EngineSpec {
	const base = "https://github.com/leejet/stable-diffusion.cpp/releases/download/master-889-c678dfe/"
	zipball := func(name string, backend catalog.Backend, bytes int64, sha256, binary string) *catalog.EngineSpec {
		return &catalog.EngineSpec{
			Name:    "stable-diffusion.cpp",
			URL:     base + name + ".zip",
			Bytes:   bytes,
			SHA256:  sha256,
			Archive: name + ".zip",
			Kind:    catalog.Zip,
			DirName: catalog.EngineDir(sdVersion, backend),
			Backend: backend,
			Binary:  binary,
		}
	}
	linux := func(eng *catalog.EngineSpec) *catalog.EngineSpec {
		eng.MinGlibc = "2.38"
		return eng
	}
	cuda := zipball("sd-master-c678dfe-bin-win-cuda12-x64", catalog.CUDA,
		332_970_080, "caa31c81523613fa02f6af4c7a52f733c8fd406cfa081a15656a0555f1abc55d", "sd-cli.exe")
	cuda.MinDriver = 551
	cuda.Parts = []catalog.EnginePart{{URL: base + "cudart-sd-bin-win-cu12-x64.zip", Archive: "cudart-sd-bin-win-cu12-x64.zip",
		Kind: catalog.Zip, Bytes: 563_452_046, SHA256: "fe20366827d357c00797eebb58244dddab7fd9a348d70090c3871004c320f38d"}}
	return map[string][]*catalog.EngineSpec{
		"darwin/arm64": {zipball("sd-master-c678dfe-bin-Darwin-macOS-26.6.2-arm64", catalog.Metal,
			34_310_861, "935f47067941d3fe095d80751f04c59cd8105e297177b98d559d9be1d9e7cfd8", "sd-cli")},
		// Both built on Ubuntu 24.04: glibc 2.38 or newer.
		"linux/amd64": {
			linux(zipball("sd-master-c678dfe-bin-Linux-Ubuntu-24.04-x86_64", catalog.CPU,
				25_417_929, "1d8dc3ecd046a666957b5775712a6f81fded1a5bc57a981e4c0c401a04fca28c", "sd-cli")),
			linux(zipball("sd-master-c678dfe-bin-Linux-Ubuntu-24.04-x86_64-vulkan", catalog.Vulkan,
				38_541_221, "e9ecf8361675de79e546c967c02813a4794ac71a5e4fd7329352b3e7ed808dcc", "sd-cli")),
		},
		"windows/amd64": {
			zipball("sd-master-c678dfe-bin-win-cpu-x64", catalog.CPU,
				17_185_424, "c8fa63444741f89ced107df00f669718805397a30cfcfa0c83c3162ea8426f39", "sd-cli.exe"),
			cuda,
			zipball("sd-master-c678dfe-bin-win-vulkan-x64", catalog.Vulkan,
				31_935_490, "2ea1dad6c54c1e4fdc61a312ee8ec44f4be36a33ece13d877c896a0253587cdc", "sd-cli.exe"),
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
		return nil, fmt.Errorf("fornax does not have a pinned stable-diffusion.cpp for %s/%s yet", runtime.GOOS, runtime.GOARCH)
	}
	return eng, nil
}

// Every pinned stable-diffusion.cpp build, by GOOS/GOARCH — `fornax pins`
// audits them all.
func SDEngines() map[string][]*catalog.EngineSpec { return sdEngines }

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
	if !paths.EngineInstalled(root, eng) {
		bar := ui.NewProgress("sd engine", eng.TotalBytes())
		if err := engine.Ensure(ctx, root, eng, bar.Set); err != nil {
			return "", nil, err
		}
		fmt.Fprintf(os.Stderr, "%s engine stable-diffusion.cpp %s (%s) installed\n", ui.Green("✓"), sdVersion, eng.Backend)
	}
	if Installed(root, spec) {
		fmt.Fprintf(os.Stderr, "%s\n", ui.Dim(spec.ID+" already installed"))
	} else {
		bar := ui.NewProgress(spec.ID, spec.TotalBytes())
		if err := ensureModel(ctx, root, spec, bar.Set); err != nil {
			return "", nil, err
		}
		fmt.Fprintf(os.Stderr, "%s %s installed (%s)\n", ui.Green("✓"), ui.Bold(spec.ID), ui.Dim(ui.HumanSize(spec.TotalBytes())))
	}
	verifying := ui.Spin("verifying " + spec.ID)
	defer verifying.Stop("")
	if err := Rehash(root, spec); err != nil {
		return "", nil, err
	}
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
