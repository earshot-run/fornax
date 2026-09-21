package main

// `say` — text to speech through `llama-tts`, a foreground tool that ships
// in the same pinned llama.cpp archive as llama-server. Qwen3-TTS clones a
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
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

func init() {
	models = append(models, modelSpec{
		id:      "qwen3-tts-1.7b",
		name:    "Qwen3-TTS 1.7B",
		summary: "Speaks text in 10 languages; clones a voice from a reference take.",
		kind:    modalSpeech,
		repo:    "ggml-org/Qwen3-TTS-12Hz-1.7B-Base-GGUF",
		model: filePin{
			file:     "Qwen3-TTS-12Hz-1.7B-Base-Q8_0.gguf",
			revision: "ca27d74bc954b73dadab5b71ca265d87fc861a7c",
			bytes:    1_847_874_400,
			sha256:   "ac7931aeb2e7aad1a6ed6602d353a5679c9d096b18ce8204ac730a8408d572e1",
		},
		mmproj: &filePin{
			file:     "mmproj-Qwen3-TTS-12Hz-1.7B-Base-Q8_0.gguf",
			revision: "ca27d74bc954b73dadab5b71ca265d87fc861a7c",
			bytes:    446_422_912,
			sha256:   "6fd65188839bcd6ecc91b277ad471e22a0edfada4699a0fe82f1165c18cfcce2",
		},
		port: 7347,
	})
}

// `fornax say <model> "text" [-o out.wav|-] [-voice ref.wav] [-lang en] [-n frames]`
func cmdSay(ctx context.Context, args []string) error {
	set := flag.NewFlagSet("say", flag.ExitOnError)
	out := set.String("o", "", "output WAV path, or - for stdout (default: <model>-<timestamp>.wav)")
	voice := set.String("voice", "", "reference audio to clone the voice from (wav/mp3/flac)")
	lang := set.String("lang", "en", "en zh de it pt es ja ko fr ru")
	frames := set.Int("n", 0, "cap output length in audio frames (default: model decides)")
	usageLine := `usage: fornax say <model> "text" [-o out.wav] [-voice ref.wav] [-lang en] [-n frames]`
	set.Usage = func() { fmt.Fprintln(os.Stderr, usageLine) }
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
	if prompt == "" && !isTTY(os.Stdin) {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return err
		}
		prompt = strings.TrimSpace(string(data))
	}
	if id == "" || prompt == "" || len(rest) > 0 {
		return fmt.Errorf("%s", usageLine)
	}
	spec := model(id)
	if spec == nil {
		return unknownModel(id)
	}
	if spec.kind != modalSpeech {
		return fmt.Errorf("%s does not speak — pick a speech model (`list`)", spec.id)
	}
	eng := engine()
	if eng == nil {
		return fmt.Errorf("fornax does not have a pinned llama.cpp for %s/%s yet", runtime.GOOS, runtime.GOARCH)
	}
	if *voice != "" {
		if info, err := os.Stat(*voice); err != nil || info.IsDir() {
			return fmt.Errorf("-voice %s is not a readable audio file", *voice)
		}
	}
	outPath := *out
	if outPath == "" {
		outPath = fmt.Sprintf("%s-%d.wav", spec.id, time.Now().Unix())
	}
	root := home()
	if err := pull(ctx, spec, eng); err != nil {
		return err
	}
	verifying := spin("verifying " + spec.id)
	if err := rehash(root, spec); err != nil {
		verifying.stop("")
		return err
	}
	verifying.stop("")
	return runSay(ctx, root, eng, spec, prompt, outPath, *voice, *lang, *frames)
}

func runSay(ctx context.Context, root string, eng *engineSpec, spec *modelSpec, prompt, outPath, voice, lang string, frames int) error {
	binary := engineBinary(root, eng, eng.tts)
	if info, err := os.Stat(binary); err != nil || info.IsDir() {
		return fmt.Errorf("the pinned llama.cpp is missing %s — reinstall the engine", eng.tts)
	}
	toStdout := outPath == "-"
	work := outPath
	if toStdout {
		tmp, err := os.CreateTemp("", "fornax-say-*.wav")
		if err != nil {
			return err
		}
		work = tmp.Name()
		tmp.Close()
		defer os.Remove(work)
	}
	ttsArgs := []string{
		"-m", modelFinal(root, spec),
		"-mm", filePath(root, spec, spec.mmproj),
		"-p", prompt,
		"--output", work,
		"--tts-lang", lang,
	}
	if voice != "" {
		ttsArgs = append(ttsArgs, "--tts-speaker-file", voice)
	}
	if frames > 0 {
		ttsArgs = append(ttsArgs, "-n", strconv.Itoa(frames))
	}
	cmd := exec.CommandContext(ctx, binary, ttsArgs...)
	cmd.Dir = filepath.Dir(binary)
	cmd.Env = []string{
		"HOME=" + filepath.Join(root, "server-home"),
		"PATH=/usr/bin:/bin:/usr/sbin:/sbin",
	}
	if tmp := os.Getenv("TMPDIR"); tmp != "" {
		cmd.Env = append(cmd.Env, "TMPDIR="+tmp)
	}
	cmd.Stdin = nil
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	fmt.Fprintf(os.Stderr, "%s\n", dim("synthesizing with "+spec.id))
	started := time.Now()
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("llama-tts exited: %w", err)
	}
	elapsed := time.Since(started)
	info, err := os.Stat(work)
	if err != nil || info.Size() == 0 {
		return fmt.Errorf("llama-tts finished but wrote no audio")
	}
	if toStdout {
		f, err := os.Open(work)
		if err != nil {
			return err
		}
		defer f.Close()
		if _, err := io.Copy(os.Stdout, f); err != nil {
			return fmt.Errorf("could not write the WAV to stdout: %w", err)
		}
		return nil
	}
	fmt.Printf("%s wrote %s (%.1fs)\n", green("✓"), outPath, elapsed.Seconds())
	return nil
}

// A speech model's `test` is a one-line synth — proves the codec path end
// to end without needing a speaker file.
func runSayTest(ctx context.Context, spec *modelSpec, eng *engineSpec) error {
	root := home()
	if err := pull(ctx, spec, eng); err != nil {
		return err
	}
	verifying := spin("verifying " + spec.id)
	if err := rehash(root, spec); err != nil {
		verifying.stop("")
		return err
	}
	verifying.stop("")
	tmp, err := os.CreateTemp("", "fornax-say-test-*.wav")
	if err != nil {
		return err
	}
	work := tmp.Name()
	tmp.Close()
	defer os.Remove(work)
	started := time.Now()
	if err := runSayQuiet(ctx, root, eng, spec, work); err != nil {
		return err
	}
	info, err := os.Stat(work)
	if err != nil || info.Size() == 0 {
		return fmt.Errorf("llama-tts wrote no audio")
	}
	fmt.Printf("%s %s — synthesized %s of audio in %.1fs\n",
		green("✓"), bold(spec.id), dim(humanSize(info.Size())), time.Since(started).Seconds())
	return nil
}

func runSayQuiet(ctx context.Context, root string, eng *engineSpec, spec *modelSpec, out string) error {
	binary := engineBinary(root, eng, eng.tts)
	cmd := exec.CommandContext(ctx, binary,
		"-m", modelFinal(root, spec),
		"-mm", filePath(root, spec, spec.mmproj),
		"-p", "Reply with exactly: ok",
		"--output", out,
		"--tts-lang", "en")
	cmd.Dir = filepath.Dir(binary)
	cmd.Env = []string{
		"HOME=" + filepath.Join(root, "server-home"),
		"PATH=/usr/bin:/bin:/usr/sbin:/sbin",
	}
	cmd.Stdin = nil
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("llama-tts exited: %w", err)
	}
	return nil
}
