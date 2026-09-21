package main

// `draw` — one-shot image generation through a pinned stable-diffusion.cpp
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
)

// The pinned stable-diffusion.cpp release (github.com/leejet). Sizes and
// digests are the zip archives themselves; upstream ships no darwin/amd64,
// linux/arm64 or windows/arm64 assets, so those platforms get no engine.
const sdVersion = "sd-master-c678dfe"

func engineSD() *engineSpec {
	const base = "https://github.com/leejet/stable-diffusion.cpp/releases/download/master-889-c678dfe/"
	zipball := func(name string, bytes int64, sha256, binary string) *engineSpec {
		return &engineSpec{
			url:     base + name + ".zip",
			bytes:   bytes,
			sha256:  sha256,
			archive: name + ".zip",
			kind:    archiveZip,
			binary:  binary,
		}
	}
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "darwin/arm64":
		return zipball("sd-master-c678dfe-bin-Darwin-macOS-26.6.2-arm64",
			34_310_861, "935f47067941d3fe095d80751f04c59cd8105e297177b98d559d9be1d9e7cfd8", "sd-cli")
	case "linux/amd64":
		return zipball("sd-master-c678dfe-bin-Linux-Ubuntu-24.04-x86_64",
			25_417_929, "1d8dc3ecd046a666957b5775712a6f81fded1a5bc57a981e4c0c401a04fca28c", "sd-cli")
	case "windows/amd64":
		return zipball("sd-master-c678dfe-bin-win-cpu-x64",
			17_185_424, "c8fa63444741f89ced107df00f669718805397a30cfcfa0c83c3162ea8426f39", "sd-cli.exe")
	}
	return nil
}

func init() {
	models = append(models, modelSpec{
		id:      "sdxl-turbo",
		name:    "SDXL Turbo",
		summary: "Text to image in a few steps — `draw` writes a PNG.",
		kind:    modalImage,
		rt:      runtimeSD,
		repo:    "stabilityai/sdxl-turbo",
		// sd-cli never serves; the port just keeps the catalog unique.
		port: 7362,
		model: filePin{
			file:     "sd_xl_turbo_1.0_fp16.safetensors",
			revision: "71153311d3dbb46851df1931d3ca6e939de83304",
			bytes:    6_938_081_905,
			sha256:   "e869ac7d6942cb327d68d5ed83a40447aadf20e0c3358d98b2cc9e270db0da26",
		},
	})
}

func sdEngineDir(root string) string {
	return filepath.Join(root, "engine", sdVersion)
}

// The engine receipt pins the archive digest; the binary check pins presence.
func sdInstalled(root string, eng *engineSpec) bool {
	info, err := os.Stat(filepath.Join(sdEngineDir(root), filepath.FromSlash(eng.binary)))
	if err != nil || info.IsDir() {
		return false
	}
	data, err := os.ReadFile(filepath.Join(sdEngineDir(root), receipt))
	return err == nil && strings.TrimSpace(string(data)) == eng.sha256
}

func ensureSDEngine(ctx context.Context, root string, eng *engineSpec, progress func(int64)) error {
	if sdInstalled(root, eng) {
		return nil
	}
	archive := enginePart(root, eng)
	if err := fetch(ctx, eng.url, eng.bytes, archive, progress); err != nil {
		return err
	}
	if err := verify(archive, eng.bytes, eng.sha256); err != nil {
		return err
	}
	return installSDEngine(root, eng, archive)
}

// Same dance as installEngine: unpack to staging, check the binary is there,
// rename into place, write the receipt. The .engine-sd-* prefix is what
// `clean` already reaps.
func installSDEngine(root string, eng *engineSpec, archive string) error {
	if err := protectDir(root); err != nil {
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
	if err := unpackZip(archive, staging); err != nil {
		return err
	}
	if info, statErr := os.Stat(filepath.Join(staging, filepath.FromSlash(eng.binary))); statErr != nil || info.IsDir() {
		return fmt.Errorf("the pinned stable-diffusion.cpp archive is missing %s", eng.binary)
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
	if err := writeEngineReceipt(finalDir, eng.sha256); err != nil {
		return err
	}
	os.Remove(archive)
	return nil
}

// "WxH" → pixels. SDXL Turbo was trained at 512x512 and wants multiples of 8.
func drawSize(s string) (int, int, error) {
	w, h, ok := strings.Cut(s, "x")
	width, errW := strconv.Atoi(w)
	height, errH := strconv.Atoi(h)
	if !ok || errW != nil || errH != nil || width <= 0 || height <= 0 || width > 4096 || height > 4096 {
		return 0, 0, fmt.Errorf("-size wants WxH like 512x512, got %q", s)
	}
	return width, height, nil
}

func cmdDraw(ctx context.Context, args []string) error {
	set := flag.NewFlagSet("draw", flag.ExitOnError)
	out := set.String("o", "", "output PNG path (default: <model>-<timestamp>.png in cwd)")
	steps := set.Int("steps", 4, "sample steps — sdxl-turbo is built for 1-4")
	seed := set.Int64("seed", -1, "RNG seed (default: random every draw)")
	size := set.String("size", "512x512", "image size WxH")
	neg := set.String("neg", "", "negative prompt")
	usageLine := `usage: fornax draw <model> "prompt" [-o out.png] [-steps N] [-seed N] [-size WxH] [-neg "prompt"]`
	set.Usage = func() { fmt.Fprintln(os.Stderr, usageLine) }
	// `draw <model> "prompt" -flags` reads naturally; flag.Parse needs flags
	// first, so the two leading positionals are pulled off like `run` does.
	var id, prompt string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		id, args = args[0], args[1:]
	}
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		prompt, args = args[0], args[1:]
	}
	set.Parse(args)
	rest := set.Args()
	if id == "" && len(rest) > 0 {
		id, rest = rest[0], rest[1:]
	}
	if prompt == "" && len(rest) > 0 {
		prompt, rest = rest[0], rest[1:]
	}
	if id == "" || prompt == "" || len(rest) > 0 {
		return fmt.Errorf("%s", usageLine)
	}
	spec := model(id)
	if spec == nil {
		return unknownModel(id)
	}
	if spec.rt != runtimeSD {
		return fmt.Errorf("%s does not draw — pick an image model (`list`)", spec.id)
	}
	eng := engineSD()
	if eng == nil {
		return fmt.Errorf("fornax does not have a pinned stable-diffusion.cpp for %s/%s yet", runtime.GOOS, runtime.GOARCH)
	}
	width, height, err := drawSize(*size)
	if err != nil {
		return err
	}
	outPath := *out
	if outPath == "" {
		outPath = fmt.Sprintf("%s-%d.png", spec.id, time.Now().Unix())
	}

	// pull() only knows the llama engine; the same steps run here against
	// the sd engine dir.
	root := home()
	if err := protectDir(root); err != nil {
		return err
	}
	if !sdInstalled(root, eng) {
		bar := newProgress("sd engine", eng.bytes)
		if err := ensureSDEngine(ctx, root, eng, bar.set); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "%s engine stable-diffusion.cpp %s installed\n", green("✓"), sdVersion)
	}
	if modelInstalled(root, spec) {
		fmt.Fprintf(os.Stderr, "%s\n", dim(spec.id+" already installed"))
	} else {
		bar := newProgress(spec.id, spec.totalBytes())
		if err := ensureModel(ctx, root, spec, bar.set); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "%s %s installed (%s)\n", green("✓"), bold(spec.id), dim(humanSize(spec.totalBytes())))
	}
	verifying := spin("verifying " + spec.id)
	if err := rehash(root, spec); err != nil {
		verifying.stop("")
		return err
	}
	verifying.stop("")
	return runDraw(ctx, root, eng, spec, prompt, *neg, outPath, width, height, *steps, *seed)
}

func runDraw(ctx context.Context, root string, eng *engineSpec, spec *modelSpec, prompt, neg, outPath string, width, height, steps int, seed int64) error {
	binary := filepath.Join(sdEngineDir(root), filepath.FromSlash(eng.binary))
	sdArgs := []string{
		"-m", modelFinal(root, spec),
		"-p", prompt,
		"-o", outPath,
		"-W", strconv.Itoa(width),
		"-H", strconv.Itoa(height),
		"--steps", strconv.Itoa(steps),
		"--seed", strconv.FormatInt(seed, 10),
		// SDXL Turbo convention: near-zero guidance, ancestral Euler.
		"--cfg-scale", "1.0",
		"--sampling-method", "euler_a",
	}
	if neg != "" {
		sdArgs = append(sdArgs, "-n", neg)
	}
	cmd := exec.CommandContext(ctx, binary, sdArgs...)
	// sd-cli loads its shared libs from beside the binary.
	cmd.Dir = filepath.Dir(binary)
	// Same scrub as spawnServer: no inherited credentials or injection vars.
	cmd.Env = []string{
		"HOME=" + filepath.Join(root, "server-home"),
		"PATH=/usr/bin:/bin:/usr/sbin:/sbin",
	}
	if tmp := os.Getenv("TMPDIR"); tmp != "" {
		cmd.Env = append(cmd.Env, "TMPDIR="+tmp)
	}
	cmd.Stdin = nil
	// sd-cli narrates every step; keep draw's stdout to just the result line.
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	fmt.Fprintf(os.Stderr, "%s\n", dim(fmt.Sprintf("drawing %dx%d with %s", width, height, spec.id)))
	started := time.Now()
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("sd-cli exited: %w", err)
	}
	elapsed := time.Since(started)
	info, err := os.Stat(outPath)
	if err != nil || info.Size() == 0 {
		return fmt.Errorf("sd-cli finished but did not write %s", outPath)
	}
	fmt.Printf("%s wrote %s (%dx%d, %.1fs)\n", green("✓"), outPath, width, height, elapsed.Seconds())
	return nil
}
