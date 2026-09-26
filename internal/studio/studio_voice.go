package studio

// Studio's speech kind: text in, WAV out through llama-tts, optionally in a
// voice cloned from a reference clip.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/earshot-run/fornax/internal/catalog"
	"github.com/earshot-run/fornax/internal/modelrt"
)

// What llama-tts takes for --tts-lang (see `fornax say -lang`).
var ttsLangs = []string{"en", "zh", "de", "it", "pt", "es", "ja", "ko", "fr", "ru"}

// Long enough for a paragraph; past this a run takes minutes and the model
// drifts, so split it instead.
const studioSpeechMaxChars = 2000

func (s *studio) speechKind() studioKind {
	return studioKind{
		modelKind: catalog.Speech,
		ext:       "wav",
		validate:  validateSpeech,
		generate:  s.speechGenerate,
	}
}

func validateSpeech(req *studioRequest) error {
	if req.Lang == "" {
		req.Lang = "en"
	}
	if !slices.Contains(ttsLangs, req.Lang) {
		return fmt.Errorf("language %q is not one of %s", req.Lang, strings.Join(ttsLangs, " "))
	}
	if n := utf8.RuneCountInString(req.Prompt); n > studioSpeechMaxChars {
		return fmt.Errorf("text is %d characters — at most %d per clip", n, studioSpeechMaxChars)
	}
	// llama-tts decodes the speaker clip with miniaudio: WAV, MP3 or FLAC.
	if req.Voice != "" && !strings.HasSuffix(req.Voice, ".wav") && !strings.HasSuffix(req.Voice, ".mp3") {
		return errors.New("a voice clip must be WAV or MP3")
	}
	req.Negative, req.Width, req.Height, req.Steps, req.Frames, req.Refs = "", 0, 0, 0, 0, nil
	return nil
}

var (
	// `init: embeddings required …` is the last line before decoding starts.
	ttsSpeakingMarker = "embeddings required"
	ttsDonePattern    = regexp.MustCompile(`generated (\d+) frames`)
)

// Folds one line of llama-tts output into the job's phase.
func (s *studio) speechProgress(job *studioJob, line string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case strings.Contains(line, ttsSpeakingMarker):
		job.Phase = "speaking"
	case ttsDonePattern.MatchString(line):
		job.Phase = "saving"
	default:
		return
	}
	s.notifyLocked()
}

// Install what the model needs, then one llama-tts run into the library.
func (s *studio) speechGenerate(ctx context.Context, job *studioJob) error {
	spec, eng, err := modelrt.Resolve(ctx, job.Model)
	if err != nil {
		return err
	}
	if eng == nil || eng.TTS == "" {
		return fmt.Errorf("%s does not speak through the llama.cpp engine", spec.ID)
	}
	if err := modelrt.Pull(ctx, spec, eng); err != nil {
		return err
	}
	voice := ""
	if job.Voice != "" {
		if voice = s.refPath(job.Voice); voice == "" {
			return fmt.Errorf("voice clip %s is gone — add it again", job.Voice)
		}
	}
	s.setPhase(job, "loading")
	out := s.outputPath(&job.studioItem)
	cmd, err := modelrt.TTSCommand(ctx, s.root, eng, spec, job.Prompt, out, voice, job.Lang, 0)
	if err != nil {
		return err
	}
	if err := s.runLogged(ctx, job, s.speechProgress, cmd.Run, func(w io.Writer) { cmd.Stdout, cmd.Stderr = w, w }); err != nil {
		return err
	}
	if info, err := os.Stat(out); err != nil || info.Size() <= 44 {
		return fmt.Errorf("llama-tts finished without writing audio: see %s", filepath.Join(s.dir, "last.log"))
	}
	return nil
}
