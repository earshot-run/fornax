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
	set.Usage = func() { fmt.Fprintln(os.Stderr, usageLine) }
	// The take leads; every other positional is question text. Same
	// pull-then-parse shape as say/draw so flags can lead instead.
	var audioPath string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		audioPath, args = args[0], args[1:]
	}
	set.Parse(args)
	rest := set.Args()
	if audioPath == "" && len(rest) > 0 {
		audioPath, rest = rest[0], rest[1:]
	}
	if audioPath == "" {
		return fmt.Errorf("%s", usageLine)
	}
	question := strings.Join(rest, " ")
	if info, err := os.Stat(audioPath); err != nil || info.IsDir() {
		return fmt.Errorf("%s is not a readable audio file", audioPath)
	}
	if *voice != "" {
		if info, err := os.Stat(*voice); err != nil || info.IsDir() {
			return fmt.Errorf("-voice %s is not a readable audio file", *voice)
		}
	}
	part, err := audioPart(audioPath)
	if err != nil {
		return err
	}
	root := home()
	pick := func(flagID, flagName, label string, kinds ...modality) (*modelSpec, *engineSpec, error) {
		spec, err := talkSpec(root, flagID, flagName, label, kinds...)
		if err != nil {
			return nil, nil, err
		}
		return resolve(spec.id)
	}
	sttSpec, sttEng, err := pick(*sttID, "stt", "audio", modalAudio)
	if err != nil {
		return err
	}
	// Only text and vision models chat — kev speaks /v1/systemone and the
	// embed/rerank/image/speech kinds don't take prompts at all.
	llmSpec, llmEng, err := pick(*llmID, "llm", "text or vision", modalText, modalVision)
	if err != nil {
		return err
	}
	ttsSpec, ttsEng, err := pick(*ttsID, "tts", "speech", modalSpeech)
	if err != nil {
		return err
	}
	if ttsEng == nil {
		return fmt.Errorf("%s does not speak through the pinned llama.cpp engine", ttsSpec.id)
	}

	var transcript string
	if err := withServer(ctx, sttSpec, sttEng, func(url, key string) error {
		started := time.Now()
		reply, err := chatOnce(ctx, url, key, sttSpec.id, []message{{
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
			return fmt.Errorf("%s returned an empty transcript", sttSpec.id)
		}
		fmt.Fprintf(os.Stderr, "%s\n", dim(fmt.Sprintf("heard %q (%.1fs)", talkPreview(transcript, 60), time.Since(started).Seconds())))
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
		reply, err := chatOnce(ctx, url, key, llmSpec.id,
			[]message{textMessage("user", prompt)}, -1)
		if err != nil {
			return err
		}
		answer = strings.TrimSpace(reply.Text)
		if answer == "" {
			return fmt.Errorf("%s returned an empty reply", llmSpec.id)
		}
		fmt.Fprintf(os.Stderr, "%s\n", dim(fmt.Sprintf("reply %q (%.1fs)", talkPreview(answer, 60), time.Since(started).Seconds())))
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
	verifying := spin("verifying " + ttsSpec.id)
	if err := rehash(root, ttsSpec); err != nil {
		verifying.stop("")
		return err
	}
	verifying.stop("")
	return runSay(ctx, root, ttsEng, ttsSpec, answer, outPath, *voice, "en", 0)
}

// The spec for one pipeline leg: the -flag's model validated for the leg's
// kind, or the first installed spec that fits when the flag is empty.
func talkSpec(root, flagID, flagName, label string, kinds ...modality) (*modelSpec, error) {
	fits := func(spec *modelSpec) bool {
		for _, kind := range kinds {
			if spec.kind == kind {
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
			return nil, fmt.Errorf("-%s %s is %s, not %s — `fornax list` shows kinds", flagName, spec.id, spec.kind, label)
		}
		return spec, nil
	}
	var first *modelSpec
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
		return nil, fmt.Errorf("no installed %s model — `fornax pull %s` first (or -%s <model>)", label, first.id, flagName)
	}
	return nil, fmt.Errorf("the catalog has no %s model", label)
}

// For default picks apple-fm counts as installed — it ships in the OS and
// its bridge compiles on pull — but only where it can actually run.
func talkInstalled(root string, spec *modelSpec) bool {
	if spec.rt == runtimeApple {
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
