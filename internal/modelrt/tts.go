package modelrt

// Text to speech through `llama-tts`, a foreground tool that ships in the
// same llama.cpp build as llama-server. Qwen3-TTS clones a voice
// from a few seconds of reference audio (--tts-speaker-file).

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"github.com/earshot-run/fornax/internal/catalog"
	"github.com/earshot-run/fornax/internal/paths"
	"github.com/earshot-run/fornax/internal/ui"
)

// Install a speech model: what every llama-tts run needs first.
func PrepareSpeech(ctx context.Context, spec *catalog.Spec, eng *catalog.EngineSpec) (string, error) {
	root := paths.Home()
	if err := Pull(ctx, spec, eng); err != nil {
		return "", err
	}
	return root, nil
}

// Synthesize to outPath ("-" streams the WAV to stdout) and report it.
func RunSay(ctx context.Context, root string, eng *catalog.EngineSpec, spec *catalog.Spec, prompt, outPath, voice, lang string, frames int) error {
	binary := paths.EngineBinary(root, eng, eng.TTS)
	if info, err := os.Stat(binary); err != nil || info.IsDir() {
		return fmt.Errorf("the installed llama.cpp is missing %s — reinstall the engine", eng.TTS)
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
	fmt.Fprintf(os.Stderr, "%s\n", ui.Dim("synthesizing with "+spec.ID))
	started := time.Now()
	if err := Synthesize(ctx, root, eng, spec, prompt, work, voice, lang, frames); err != nil {
		return err
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
	fmt.Printf("%s wrote %s (%.1fs)\n", ui.Green("✓"), outPath, elapsed.Seconds())
	return nil
}

// One llama-tts run: every caller synthesizes to a file and narrates on
// stderr, so stdout stays whatever the command itself writes.
func Synthesize(ctx context.Context, root string, eng *catalog.EngineSpec, spec *catalog.Spec, prompt, out, voice, lang string, frames int) error {
	cmd, err := TTSCommand(ctx, root, eng, spec, prompt, out, voice, lang, frames)
	if err != nil {
		return err
	}
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

// The llama-tts invocation for one synthesis; the caller wires its output.
func TTSCommand(ctx context.Context, root string, eng *catalog.EngineSpec, spec *catalog.Spec, prompt, out, voice, lang string, frames int) (*exec.Cmd, error) {
	if spec.MMProj == nil {
		return nil, fmt.Errorf("%s has no speech projector; add one with fornax pull --mmproj", spec.ID)
	}
	out, err := filepath.Abs(out)
	if err != nil {
		return nil, err
	}
	if voice != "" {
		voice, err = filepath.Abs(voice)
		if err != nil {
			return nil, err
		}
	}
	binary := paths.EngineBinary(root, eng, eng.TTS)
	args := []string{
		"-m", paths.ModelFinal(root, spec),
		"-mm", paths.FilePath(root, spec, spec.MMProj),
		"-p", prompt,
		"--output", out,
		"--tts-lang", lang,
	}
	if voice != "" {
		args = append(args, "--tts-speaker-file", voice)
	}
	if frames > 0 {
		args = append(args, "-n", strconv.Itoa(frames))
	}
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir = filepath.Dir(binary)
	cmd.Env = engineEnv(root, binary)
	cmd.Stdin = nil
	return cmd, nil
}
