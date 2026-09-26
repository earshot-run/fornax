package main

import (
	"context"
	"strings"
	"testing"
)

// Non-image models are refused before anything is fetched.
func TestDrawRejectsTextModel(t *testing.T) {
	err := cmdImagine(context.Background(), []string{"apple-fm", "a door"})
	if err == nil || !strings.Contains(err.Error(), "does not imagine") {
		t.Fatalf("expected a does-not-imagine error, got %v", err)
	}
}

func TestDrawUnknownModel(t *testing.T) {
	err := cmdImagine(context.Background(), []string{"nope-0b", "a door"})
	if err == nil || !strings.Contains(err.Error(), "unknown model") {
		t.Fatalf("expected an unknown-model error, got %v", err)
	}
}

func TestDrawNeedsModelAndPrompt(t *testing.T) {
	for _, args := range [][]string{
		{},
		{"some-model"},
	} {
		if err := cmdImagine(context.Background(), args); err == nil {
			t.Fatalf("args %v: expected a usage error", args)
		}
	}
}

func TestDrawSizeParses(t *testing.T) {
	if w, h, err := drawSize("768x512"); err != nil || w != 768 || h != 512 {
		t.Fatalf("drawSize(768x512) = %d, %d, %v", w, h, err)
	}
	for _, bad := range []string{"", "512", "x512", "512x", "axb", "-1x512", "0x0", "8192x8192"} {
		if _, _, err := drawSize(bad); err == nil {
			t.Fatalf("drawSize(%q) should fail", bad)
		}
	}
}

func TestSDEnginePinIsComplete(t *testing.T) {
	eng := engineSD()
	if eng == nil {
		t.Skip("unsupported platform")
	}
	if len(eng.SHA256) != 64 || eng.Bytes <= 0 || eng.Binary == "" {
		t.Fatal("incomplete sd engine pin")
	}
	if !strings.HasSuffix(eng.URL, eng.Archive) {
		t.Fatal("url does not end with the pinned archive")
	}
}

// A user-added model runs with the files and arguments saved beside it, and
// a flag given on the command line comes last so it wins.
func TestSDArgsCarryTheSavedRecipe(t *testing.T) {
	entry := customEntry{
		ID: "my-video", Kind: "video", Repo: "org/repo", File: "video.gguf",
		Companions: []customCompanion{{Flag: "vae", File: "vae.safetensors"}, {Flag: "t5xxl", File: "umt5.gguf"}},
		Args:       []string{"--steps", "4", "--cfg-scale", "1.0"},
	}
	spec := entry.spec()
	got := strings.Join(sdArgs("/root", spec, "a boat", "out.webm", 7, []string{"--steps", "8"}), " ")
	for _, want := range []string{
		"--diffusion-model /root/models/my-video/video.gguf -M vid_gen",
		"--vae /root/models/my-video/vae.safetensors",
		"--t5xxl /root/models/my-video/umt5.gguf",
		"--steps 4 --cfg-scale 1.0 --steps 8",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("sd-cli args %q are missing %q", got, want)
		}
	}
	checkpoint := (&customEntry{ID: "my-image", Kind: "image", File: "sd.safetensors"}).spec()
	if got := sdArgs("/root", checkpoint, "a door", "out.png", 7, nil); got[0] != "-m" {
		t.Fatalf("a lone checkpoint loads with -m, got %v", got[:2])
	}
}
