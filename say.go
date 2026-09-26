package main

// `say` — text to speech through `llama-tts`, a foreground tool that ships
// in the same llama.cpp build as llama-server. Qwen3-TTS clones a
// voice from a few seconds of reference audio (--tts-speaker-file), which is
// the part of a voice studio worth salvaging: one flag, no training, no
// cloud.

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/earshot-run/fornax/internal/catalog"
	"github.com/earshot-run/fornax/internal/modelrt"
	"github.com/earshot-run/fornax/internal/paths"
	"github.com/earshot-run/fornax/internal/ui"
)

// `fornax say <model> "text" [-o out.wav|-] [-voice ref.wav] [-lang en] [-n frames]`
func cmdSay(ctx context.Context, args []string) error {
	set := flag.NewFlagSet("say", flag.ExitOnError)
	out := set.String("o", "", "output WAV path, or - for stdout (default: <model>-<timestamp>.wav)")
	voice := set.String("voice", "", "reference audio to clone the voice from (wav/mp3/flac)")
	lang := set.String("lang", "en", "en zh de it pt es ja ko fr ru")
	frames := set.Int("n", 0, "cap output length in audio frames (default: model decides)")
	usageLine := `usage: fornax say <model> "text" [-o out.wav] [-voice ref.wav] [-lang en] [-n frames]`
	set.Usage = ui.UsageFunc(set, usageLine)
	got := parseFlexible(set, args, 2)
	if len(got) > 2 {
		return fmt.Errorf("%s", usageLine)
	}
	var id, prompt string
	if len(got) > 0 {
		id = got[0]
	}
	if len(got) > 1 {
		prompt = got[1]
	}
	if prompt == "" && !ui.IsTTY(os.Stdin) {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return err
		}
		prompt = strings.TrimSpace(string(data))
	}
	if id == "" || prompt == "" {
		return fmt.Errorf("%s", usageLine)
	}
	spec, eng, err := modelrt.Resolve(ctx, id)
	if err != nil {
		return err
	}
	if spec.Kind != catalog.Speech {
		return fmt.Errorf("%s does not speak — pick a speech model (`list`)", spec.ID)
	}
	if *voice != "" {
		if info, err := os.Stat(*voice); err != nil || info.IsDir() {
			return fmt.Errorf("-voice %s is not a readable audio file", *voice)
		}
	}
	outPath := *out
	if outPath == "" {
		outPath = fmt.Sprintf("%s-%d.wav", spec.ID, time.Now().Unix())
	}
	root, err := modelrt.PrepareSpeech(ctx, spec, eng)
	if err != nil {
		return err
	}
	return modelrt.RunSay(ctx, root, eng, spec, prompt, outPath, *voice, *lang, *frames)
}

// A speech model's `test` is a one-line synth — proves the codec path end
// to end without needing a speaker file.
func runSayTest(ctx context.Context, spec *catalog.Spec, eng *catalog.EngineSpec) error {
	root, err := modelrt.PrepareSpeech(ctx, spec, eng)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp("", "fornax-say-test-*.wav")
	if err != nil {
		return err
	}
	work := tmp.Name()
	tmp.Close()
	defer os.Remove(work)
	started := time.Now()
	if err := modelrt.Synthesize(ctx, root, eng, spec, "Reply with exactly: ok", work, "", "en", 0); err != nil {
		return err
	}
	info, err := os.Stat(work)
	if err != nil || info.Size() == 0 {
		return fmt.Errorf("llama-tts wrote no audio")
	}
	fmt.Printf("%s %s — synthesized %s of audio in %.1fs\n",
		ui.Green("✓"), ui.Bold(spec.ID), ui.Dim(ui.HumanSize(info.Size())), time.Since(started).Seconds())
	return nil
}

// `chat -speak` voice: the first installed speech model, or none.
func speechSpec(root string) *catalog.Spec {
	for _, spec := range modelrt.AllSpecs(root) {
		if spec.Kind == catalog.Speech && modelrt.Installed(root, spec) {
			return spec
		}
	}
	return nil
}

// Synthesize text to a temp WAV and play it through the OS player.
func speakText(ctx context.Context, spec *catalog.Spec, text string) error {
	root := paths.Home()
	eng, err := modelrt.LlamaEngine()
	if err != nil || eng.TTS == "" {
		return fmt.Errorf("no speech engine on this platform")
	}
	tmp, err := os.CreateTemp("", "fornax-speak-*.wav")
	if err != nil {
		return err
	}
	work := tmp.Name()
	tmp.Close()
	defer os.Remove(work)
	if err := modelrt.Synthesize(ctx, root, eng, spec, text, work, "", "en", 0); err != nil {
		return err
	}
	player := "afplay"
	switch runtime.GOOS {
	case "linux":
		player = "aplay"
	case "windows":
		fmt.Printf("%s %s\n", ui.Dim("audio at"), work)
		return nil
	}
	play := exec.CommandContext(ctx, player, work)
	play.Stdout, play.Stderr = os.Stderr, os.Stderr
	return play.Run()
}
