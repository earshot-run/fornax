package main

// `imagine` and `animate` — one-shot image generation through a pinned stable-diffusion.cpp
// build (sd-cli), the same pinned-engine + pinned-weights pattern as the
// llama.cpp path. sd-cli is a foreground process, not a server: prompt in,
// PNG out, exit. It unpacks beside the llama engine under engine/<tag> so
// the two never share a dir or a receipt.

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/earshot-run/fornax/internal/catalog"
	"github.com/earshot-run/fornax/internal/download"
	"github.com/earshot-run/fornax/internal/engine"
	"github.com/earshot-run/fornax/internal/paths"
	"github.com/earshot-run/fornax/internal/ui"
)

// The pinned stable-diffusion.cpp release (github.com/leejet). Sizes and
// digests are the zip archives themselves; upstream ships no darwin/amd64,
// linux/arm64 or windows/arm64 assets, so those platforms get no engine.
const sdVersion = "sd-master-c678dfe"

var sdEngines = func() map[string]*catalog.EngineSpec {
	const base = "https://github.com/leejet/stable-diffusion.cpp/releases/download/master-889-c678dfe/"
	zipball := func(name string, bytes int64, sha256, binary string) *catalog.EngineSpec {
		return &catalog.EngineSpec{
			URL:     base + name + ".zip",
			Bytes:   bytes,
			SHA256:  sha256,
			Archive: name + ".zip",
			Kind:    catalog.Zip,
			Binary:  binary,
		}
	}
	return map[string]*catalog.EngineSpec{
		"darwin/arm64": zipball("sd-master-c678dfe-bin-Darwin-macOS-26.6.2-arm64",
			34_310_861, "935f47067941d3fe095d80751f04c59cd8105e297177b98d559d9be1d9e7cfd8", "sd-cli"),
		"linux/amd64": zipball("sd-master-c678dfe-bin-Linux-Ubuntu-24.04-x86_64",
			25_417_929, "1d8dc3ecd046a666957b5775712a6f81fded1a5bc57a981e4c0c401a04fca28c", "sd-cli"),
		"windows/amd64": zipball("sd-master-c678dfe-bin-win-cpu-x64",
			17_185_424, "c8fa63444741f89ced107df00f669718805397a30cfcfa0c83c3162ea8426f39", "sd-cli.exe"),
	}
}()

func engineSD() *catalog.EngineSpec {
	return sdEngines[runtime.GOOS+"/"+runtime.GOARCH]
}

func sdEngineDir(root string) string {
	return filepath.Join(root, "engine", sdVersion)
}

// The engine receipt pins the archive digest; the binary check pins presence.
func sdInstalled(root string, eng *catalog.EngineSpec) bool {
	info, err := os.Stat(filepath.Join(sdEngineDir(root), filepath.FromSlash(eng.Binary)))
	if err != nil || info.IsDir() {
		return false
	}
	data, err := os.ReadFile(filepath.Join(sdEngineDir(root), paths.Receipt))
	return err == nil && strings.TrimSpace(string(data)) == eng.SHA256
}

func ensureSDEngine(ctx context.Context, root string, eng *catalog.EngineSpec, progress func(int64)) error {
	if sdInstalled(root, eng) {
		return nil
	}
	archive := paths.EnginePart(root, eng)
	if err := download.Fetch(ctx, eng.URL, eng.Bytes, archive, progress); err != nil {
		return err
	}
	if err := download.Verify(archive, eng.Bytes, eng.SHA256); err != nil {
		return err
	}
	return installSDEngine(root, eng, archive)
}

// Same dance as installEngine: unpack to staging, check the binary is there,
// rename into place, write the receipt. The .engine-sd-* prefix is what
// `clean` already reaps.
func installSDEngine(root string, eng *catalog.EngineSpec, archive string) error {
	if err := paths.ProtectDir(root); err != nil {
		return err
	}
	staging, err := os.MkdirTemp(root, ".engine-sd-")
	if err != nil {
		return fmt.Errorf("could not stage stable-diffusion.cpp: %w", err)
	}
	cleanup := true
	defer func() {
		if cleanup {
			os.RemoveAll(staging)
		}
	}()
	if err := engine.UnpackZip(archive, staging); err != nil {
		return err
	}
	if info, statErr := os.Stat(filepath.Join(staging, filepath.FromSlash(eng.Binary))); statErr != nil || info.IsDir() {
		return fmt.Errorf("the pinned stable-diffusion.cpp archive is missing %s", eng.Binary)
	}
	finalDir := sdEngineDir(root)
	if err := os.RemoveAll(finalDir); err != nil {
		return fmt.Errorf("could not replace managed stable-diffusion.cpp: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(finalDir), 0o700); err != nil {
		return fmt.Errorf("could not install stable-diffusion.cpp: %w", err)
	}
	if err := os.Rename(staging, finalDir); err != nil {
		return fmt.Errorf("could not install stable-diffusion.cpp: %w", err)
	}
	cleanup = false
	if err := paths.WriteEngineReceipt(finalDir, eng.SHA256); err != nil {
		return err
	}
	os.Remove(archive)
	return nil
}

// "WxH" → pixels.
func drawSize(s string) (int, int, error) {
	w, h, ok := strings.Cut(s, "x")
	width, errW := strconv.Atoi(w)
	height, errH := strconv.Atoi(h)
	if !ok || errW != nil || errH != nil || width <= 0 || height <= 0 || width > 4096 || height > 4096 {
		return 0, 0, fmt.Errorf("-size wants WxH like 512x512, got %q", s)
	}
	return width, height, nil
}

// fornax pins the engine and nothing else: an image or video model is one
// the operator added with `pull hf:`, with the files and engine arguments
// they saved beside it. A flag given here is passed after those, so it wins.
func cmdImagine(ctx context.Context, args []string) error {
	return cmdSD(ctx, "imagine", catalog.Image, "png", args)
}

func cmdAnimate(ctx context.Context, args []string) error {
	return cmdSD(ctx, "animate", catalog.Video, "webm", args)
}

func cmdSD(ctx context.Context, verb string, kind catalog.Modality, ext string, args []string) error {
	set := flag.NewFlagSet(verb, flag.ExitOnError)
	out := set.String("o", "", "output path (default: <model>-<timestamp>."+ext+" in cwd)")
	image := set.String("image", "", "image to start from")
	steps := set.Int("steps", 0, "sample steps")
	seed := set.Int64("seed", -1, "RNG seed (default: random)")
	size := set.String("size", "", "size WxH")
	neg := set.String("neg", "", "negative prompt")
	frames := set.Int("frames", 0, "video frames")
	usageLine := "usage: fornax " + verb + ` <model> "prompt" [-o out.` + ext + `] [-image in.png] [-size WxH] [-steps N] [-seed N] [-neg "prompt"]`
	if kind == catalog.Video {
		usageLine += " [-frames N]"
	}
	set.Usage = ui.UsageFunc(set, usageLine)
	got := parseFlexible(set, args, 2)
	if len(got) != 2 {
		return fmt.Errorf("%s", usageLine)
	}
	id, prompt := got[0], got[1]
	spec := model(id)
	if spec == nil {
		return unknownModel(id)
	}
	if spec.Runtime != catalog.SD || spec.Kind != kind {
		return fmt.Errorf("%s does not %s — add a model with `fornax pull hf:… --kind %s`", spec.ID, verb, kind)
	}
	var extra []string
	if *size != "" {
		width, height, err := drawSize(*size)
		if err != nil {
			return err
		}
		extra = append(extra, "-W", strconv.Itoa(width), "-H", strconv.Itoa(height))
	}
	if *steps > 0 {
		extra = append(extra, "--steps", strconv.Itoa(*steps))
	}
	if *frames > 0 {
		extra = append(extra, "--video-frames", strconv.Itoa(*frames))
	}
	if *neg != "" {
		extra = append(extra, "-n", *neg)
	}
	if *image != "" {
		extra = append(extra, "-i", *image)
	}
	outPath := *out
	if outPath == "" {
		outPath = fmt.Sprintf("%s-%d.%s", spec.ID, time.Now().Unix(), ext)
	}
	root, eng, err := prepareSD(ctx, spec)
	if err != nil {
		return err
	}
	return runSD(ctx, root, eng, spec, prompt, outPath, *seed, extra)
}

// pull() only knows the llama engine; the same steps run here against the
// sd engine dir.
func prepareSD(ctx context.Context, spec *catalog.Spec) (string, *catalog.EngineSpec, error) {
	eng := engineSD()
	if eng == nil {
		return "", nil, fmt.Errorf("fornax does not have a pinned stable-diffusion.cpp for %s/%s yet", runtime.GOOS, runtime.GOARCH)
	}
	root := paths.Home()
	if err := paths.ProtectDir(root); err != nil {
		return "", nil, err
	}
	if !sdInstalled(root, eng) {
		bar := ui.NewProgress("sd engine", eng.Bytes)
		if err := ensureSDEngine(ctx, root, eng, bar.Set); err != nil {
			return "", nil, err
		}
		fmt.Fprintf(os.Stderr, "%s engine stable-diffusion.cpp %s installed\n", ui.Green("✓"), sdVersion)
	}
	if modelInstalled(root, spec) {
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
	if err := rehash(root, spec); err != nil {
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

func runSD(ctx context.Context, root string, eng *catalog.EngineSpec, spec *catalog.Spec, prompt, outPath string, seed int64, extra []string) error {
	binary := filepath.Join(sdEngineDir(root), filepath.FromSlash(eng.Binary))
	cmd := exec.CommandContext(ctx, binary, sdArgs(root, spec, prompt, outPath, seed, extra)...)
	// sd-cli loads its shared libs from beside the binary.
	cmd.Dir = filepath.Dir(binary)
	cmd.Env = scrubbedEnv(root)
	cmd.Stdin = nil
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
