package main

// `talk` — speech in, speech out. Three legs: an audio model transcribes
// the take (chat completions with an input_audio part), a text or vision
// model answers the transcript, and a speech model speaks the reply through
// llama-tts. Each leg is a normal one-shot — withServer for the two chat
// calls, runSay for the foreground synth.

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/earshot-run/fornax/internal/catalog"
	"github.com/earshot-run/fornax/internal/openai"
	"github.com/earshot-run/fornax/internal/paths"
	"github.com/earshot-run/fornax/internal/ui"
)

// `fornax talk <audio> [question…] [-llm m] [-stt m] [-tts m] [-voice ref.wav] [-o out.wav|-]`
func cmdTalk(ctx context.Context, args []string) error {
	set := flag.NewFlagSet("talk", flag.ExitOnError)
	llmID := set.String("llm", "", "model that answers (default: first installed text or vision model)")
	sttID := set.String("stt", "", "model that transcribes (default: first installed audio model)")
	ttsID := set.String("tts", "", "model that speaks the reply (default: first installed speech model)")
	voice := set.String("voice", "", "reference audio to clone the reply voice from (wav/mp3/flac)")
	out := set.String("o", "", "output WAV path, or - for stdout (default: talk-<timestamp>.wav)")
	usageLine := `usage: fornax talk <audio> [question…] [-llm m] [-stt m] [-tts m] [-voice ref.wav] [-o out.wav]`
	set.Usage = ui.UsageFunc(set, usageLine)
	// The take leads; every other positional is question text.
	got := parseFlexible(set, args, 1)
	if len(got) == 0 {
		return fmt.Errorf("%s", usageLine)
	}
	audioPath := got[0]
	question := strings.Join(got[1:], " ")
	if info, err := os.Stat(audioPath); err != nil || info.IsDir() {
		return fmt.Errorf("%s is not a readable audio file", audioPath)
	}
	if *voice != "" {
		if info, err := os.Stat(*voice); err != nil || info.IsDir() {
			return fmt.Errorf("-voice %s is not a readable audio file", *voice)
		}
	}
	part, err := openai.AudioPart(audioPath)
	if err != nil {
		return err
	}
	root := paths.Home()
	pick := func(flagID, flagName, label string, kinds ...catalog.Modality) (*catalog.Spec, *catalog.EngineSpec, error) {
		spec, err := talkSpec(root, flagID, flagName, label, kinds...)
		if err != nil {
			return nil, nil, err
		}
		return resolve(ctx, spec.ID)
	}
	sttSpec, sttEng, err := pick(*sttID, "stt", "audio", catalog.Audio)
	if err != nil {
		return err
	}
	// Only text and vision models chat — kev speaks /v1/systemone and the
	// embed/rerank/image/speech kinds don't take prompts at all.
	llmSpec, llmEng, err := pick(*llmID, "llm", "text or vision", catalog.Text, catalog.Vision)
	if err != nil {
		return err
	}
	ttsSpec, ttsEng, err := pick(*ttsID, "tts", "speech", catalog.Speech)
	if err != nil {
		return err
	}
	if ttsEng == nil {
		return fmt.Errorf("%s does not speak through the pinned llama.cpp engine", ttsSpec.ID)
	}

	var transcript string
	if err := withServer(ctx, sttSpec, sttEng, func(url, key string) error {
		started := time.Now()
		reply, err := openai.Once(ctx, url, key, sttSpec.ID, []openai.Message{{
			Role: "user",
			Content: []any{
				map[string]any{"type": "text", "text": "Transcribe what is said."},
				part,
			},
		}}, -1)
		if err != nil {
			return err
		}
		transcript = strings.TrimSpace(reply.Text)
		if transcript == "" {
			return fmt.Errorf("%s returned an empty transcript", sttSpec.ID)
		}
		fmt.Fprintf(os.Stderr, "%s\n", ui.Dim(fmt.Sprintf("heard %q (%.1fs)", talkPreview(transcript, 60), time.Since(started).Seconds())))
		return nil
	}); err != nil {
		return err
	}

	prompt := transcript
	if question != "" {
		prompt = transcript + "\n\n" + question
	}
	var answer string
	if err := withServer(ctx, llmSpec, llmEng, func(url, key string) error {
		started := time.Now()
		reply, err := openai.Once(ctx, url, key, llmSpec.ID,
			[]openai.Message{openai.TextMessage("user", prompt)}, -1)
		if err != nil {
			return err
		}
		answer = strings.TrimSpace(reply.Text)
		if answer == "" {
			return fmt.Errorf("%s returned an empty reply", llmSpec.ID)
		}
		fmt.Fprintf(os.Stderr, "%s\n", ui.Dim(fmt.Sprintf("reply %q (%.1fs)", talkPreview(answer, 60), time.Since(started).Seconds())))
		return nil
	}); err != nil {
		return err
	}

	outPath := *out
	if outPath == "" {
		outPath = fmt.Sprintf("talk-%d.wav", time.Now().Unix())
	}
	if err := pull(ctx, ttsSpec, ttsEng); err != nil {
		return err
	}
	verifying := ui.Spin("verifying " + ttsSpec.ID)
	if err := rehash(root, ttsSpec); err != nil {
		verifying.Stop("")
		return err
	}
	verifying.Stop("")
	return runSay(ctx, root, ttsEng, ttsSpec, answer, outPath, *voice, "en", 0)
}

// The spec for one pipeline leg: the -flag's model validated for the leg's
// kind, or the first installed spec that fits when the flag is empty.
func talkSpec(root, flagID, flagName, label string, kinds ...catalog.Modality) (*catalog.Spec, error) {
	fits := func(spec *catalog.Spec) bool {
		for _, kind := range kinds {
			if spec.Kind == kind {
				return true
			}
		}
		return false
	}
	if flagID != "" {
		spec := model(flagID)
		if spec == nil {
			return nil, unknownModel(flagID)
		}
		if !fits(spec) {
			return nil, fmt.Errorf("-%s %s is %s, not %s — `fornax list` shows kinds", flagName, spec.ID, spec.Kind, label)
		}
		return spec, nil
	}
	var first *catalog.Spec
	for _, spec := range allSpecs(root) {
		if !fits(spec) {
			continue
		}
		if first == nil {
			first = spec
		}
		if talkInstalled(root, spec) {
			return spec, nil
		}
	}
	if first != nil {
		return nil, fmt.Errorf("no installed %s model — `fornax pull %s` first (or -%s <model>)", label, first.ID, flagName)
	}
	return nil, fmt.Errorf("no %s model is known — `fornax search` finds one to pull", label)
}

// For default picks apple-fm counts as installed — it ships in the OS and
// its bridge compiles on pull — but only where it can actually run.
func talkInstalled(root string, spec *catalog.Spec) bool {
	if spec.Runtime == catalog.Apple {
		return appleSupported() == nil
	}
	return modelInstalled(root, spec)
}

// One collapsed line, at most n runes, for the stderr status lines.
func talkPreview(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	runes := []rune(s)
	if len(runes) > n {
		return string(runes[:n-1]) + "…"
	}
	return s
}
