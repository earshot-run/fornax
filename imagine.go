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
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/earshot-run/fornax/internal/catalog"
	"github.com/earshot-run/fornax/internal/modelrt"
	"github.com/earshot-run/fornax/internal/ui"
)

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
	spec := modelrt.Model(id)
	if spec == nil {
		return modelrt.UnknownModel(id)
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
		abs, err := filepath.Abs(*image)
		if err != nil {
			return err
		}
		extra = append(extra, "-i", abs)
	}
	outPath := *out
	if outPath == "" {
		outPath = fmt.Sprintf("%s-%d.%s", spec.ID, time.Now().Unix(), ext)
	}
	root, eng, err := modelrt.PrepareSD(ctx, spec)
	if err != nil {
		return err
	}
	return modelrt.RunSD(ctx, root, eng, spec, prompt, outPath, *seed, extra)
}
