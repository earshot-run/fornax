package main

// The workbench half: the commands that *use* a model, each one a callback
// handed to modelrt.WithServer.

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/earshot-run/fornax/internal/catalog"
	"github.com/earshot-run/fornax/internal/modelrt"
	"github.com/earshot-run/fornax/internal/openai"
	"github.com/earshot-run/fornax/internal/paths"
	"github.com/earshot-run/fornax/internal/ui"
)

// One prompt, one streamed reply. The prompt comes from the args or stdin.
func runAsk(ctx context.Context, spec *catalog.Spec, eng *catalog.EngineSpec, prompt string) error {
	return modelrt.WithServer(ctx, spec, eng, func(url, key string) error {
		reply, err := openai.Stream(ctx, url, key, spec.ID,
			[]openai.Message{openai.TextMessage("user", prompt)}, -1,
			func(token string) { fmt.Print(token) })
		fmt.Println()
		if err != nil {
			return err
		}
		if reply.Text == "" {
			return fmt.Errorf("the model returned an empty reply")
		}
		return nil
	})
}

// A multi-turn REPL with history until /exit, /quit or Ctrl-D. A non-nil
// voice reads each reply aloud (`chat -speak`).
func runChat(ctx context.Context, spec *catalog.Spec, eng *catalog.EngineSpec, voice *catalog.Spec) error {
	return modelrt.WithServer(ctx, spec, eng, func(url, key string) error {
		fmt.Fprintf(os.Stderr, "%s\n", ui.Dim(spec.Name+" — type a message, /exit to leave, /clear to forget"))
		var history []openai.Message
		reader := bufio.NewReader(os.Stdin)
		interactive := ui.IsTTY(os.Stdin)
		for {
			if interactive {
				fmt.Print(ui.Cyan("› "))
			}
			line, err := reader.ReadString('\n')
			if err != nil && err != io.EOF {
				return err
			}
			line = strings.TrimSpace(line)
			// A final line without a newline still gets answered; the next
			// read hits EOF and leaves.
			if err == io.EOF && line == "" {
				fmt.Println()
				return nil
			}
			switch line {
			case "":
				continue
			case "/exit", "/quit":
				return nil
			case "/clear":
				history = history[:0]
				fmt.Fprintln(os.Stderr, ui.Dim("history cleared"))
				continue
			}
			history = append(history, openai.TextMessage("user", line))
			reply, err := openai.Stream(ctx, url, key, spec.ID, history, -1,
				func(token string) { fmt.Print(token) })
			fmt.Println()
			if err != nil {
				return err
			}
			history = append(history, openai.TextMessage("assistant", reply.Text))
			if voice != nil {
				if err := speakText(ctx, voice, reply.Text); err != nil {
					fmt.Fprintf(os.Stderr, "%s\n", ui.Dim("speech failed: "+err.Error()))
				}
			}
		}
	})
}

// Describe an image. Vision models only.
func runSee(ctx context.Context, spec *catalog.Spec, eng *catalog.EngineSpec, imagePath, question string) error {
	if spec.Kind != catalog.Vision {
		return fmt.Errorf("%s cannot see — pick a vision model (`list`)", spec.ID)
	}
	part, err := openai.ImagePart(imagePath)
	if err != nil {
		return err
	}
	return modelrt.WithServer(ctx, spec, eng, func(url, key string) error {
		msgs := []openai.Message{{
			Role: "user",
			Content: []any{
				map[string]any{"type": "text", "text": question},
				part,
			},
		}}
		reply, err := openai.Stream(ctx, url, key, spec.ID, msgs, -1,
			func(token string) { fmt.Print(token) })
		fmt.Println()
		if err != nil {
			return err
		}
		if reply.Text == "" {
			return fmt.Errorf("the model returned an empty reply")
		}
		return nil
	})
}

// Transcribe or answer about an audio take. Audio models only.
func runHear(ctx context.Context, spec *catalog.Spec, eng *catalog.EngineSpec, audioPath, question string) error {
	if spec.Kind != catalog.Audio {
		return fmt.Errorf("%s cannot hear — pick an audio model (`list`)", spec.ID)
	}
	part, err := openai.AudioPart(audioPath)
	if err != nil {
		return err
	}
	return modelrt.WithServer(ctx, spec, eng, func(url, key string) error {
		msgs := []openai.Message{{
			Role: "user",
			Content: []any{
				map[string]any{"type": "text", "text": question},
				part,
			},
		}}
		reply, err := openai.Stream(ctx, url, key, spec.ID, msgs, -1,
			func(token string) { fmt.Print(token) })
		fmt.Println()
		if err != nil {
			return err
		}
		if reply.Text == "" {
			return fmt.Errorf("the model returned an empty reply")
		}
		return nil
	})
}

// A smoke check with real numbers: does the model load and answer, and how
// fast. Reports prompt/generation speed from the server's own timings.
func runTest(ctx context.Context, spec *catalog.Spec, eng *catalog.EngineSpec) error {
	return modelrt.WithServer(ctx, spec, eng, func(url, key string) error {
		started := time.Now()
		reply, err := openai.Once(ctx, url, key, spec.ID,
			[]openai.Message{openai.TextMessage("user", "Reply with exactly: ok")}, 8)
		elapsed := time.Since(started)
		if err != nil {
			return err
		}
		if strings.TrimSpace(reply.Text) == "" {
			return fmt.Errorf("the model returned an empty reply")
		}
		fmt.Printf("%s %s — %q in %.1fs\n", ui.Green("✓"), ui.Bold(spec.ID),
			strings.TrimSpace(reply.Text), elapsed.Seconds())
		if reply.Timings != nil {
			if v, ok := reply.Timings["predicted_per_second"].(float64); ok && v > 0 {
				fmt.Printf("    generate  %.0f tok/s\n", v)
			}
			if v, ok := reply.Timings["prompt_per_second"].(float64); ok && v > 0 {
				fmt.Printf("    prompt    %.0f tok/s\n", v)
			}
		}
		return nil
	})
}

// What is serving right now, on each model's own port.
func cmdPs(args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("usage: fornax ps")
	}
	return runPs()
}

func runPs() error {
	root := paths.Home()
	cfg, err := paths.LoadConfig(root)
	if err != nil {
		return err
	}
	any := false
	for _, spec := range modelrt.AllSpecs(root) {
		if modelrt.IsServing(spec, cfg.APIKey) {
			any = true
			fmt.Printf("%s %-16s %s :%d\n", ui.MarkOK(), spec.ID, ui.Dim("serving"), spec.Port)
		}
	}
	if !any {
		fmt.Println(ui.Dim("nothing is serving — `fornax run <model>` starts one"))
	}
	return nil
}

func cmdAsk(ctx context.Context, args []string) error {
	spec, eng, rest, err := modelArgs(ctx, "ask", args, " [prompt…]  (or pipe it on stdin)")
	if err != nil {
		return err
	}
	if err := modelrt.RequireChat(spec); err != nil {
		return err
	}
	// --json / --schema sit after the model, before the prompt.
	structured := flag.NewFlagSet("ask", flag.ExitOnError)
	jsonOut := structured.Bool("json", false, "constrain the reply to a JSON object")
	schemaPath := structured.String("schema", "", "JSON Schema file to constrain the reply ('-' reads stdin)")
	structured.Usage = ui.UsageFunc(structured, "usage: fornax ask <model> [--json|--schema f] [prompt…]")
	structured.Parse(rest)
	if *jsonOut || *schemaPath != "" {
		prompt := strings.Join(structured.Args(), " ")
		if prompt == "" {
			return fmt.Errorf("usage: fornax ask <model> [--json|--schema f] <prompt…>")
		}
		jsonFlag := ""
		if *jsonOut {
			jsonFlag = "json"
		}
		return runAskStructured(ctx, spec, eng, prompt, jsonFlag, *schemaPath)
	}
	prompt := strings.Join(structured.Args(), " ")
	if prompt == "" {
		if info, err := os.Stdin.Stat(); err == nil && info.Mode()&os.ModeCharDevice != 0 {
			return fmt.Errorf("usage: fornax ask <model> <prompt…>  (or pipe text on stdin)")
		}
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return err
		}
		prompt = strings.TrimSpace(string(data))
	}
	if prompt == "" {
		return fmt.Errorf("usage: fornax ask <model> <prompt…>")
	}
	return runAsk(ctx, spec, eng, prompt)
}

func cmdChat(ctx context.Context, args []string) error {
	spec, eng, rest, err := modelArgs(ctx, "chat", args, "")
	if err != nil {
		return err
	}
	if err := modelrt.RequireChat(spec); err != nil {
		return err
	}
	chatFlags := flag.NewFlagSet("chat", flag.ExitOnError)
	speak := chatFlags.Bool("speak", false, "read replies aloud through an installed speech model")
	chatFlags.Usage = ui.UsageFunc(chatFlags, "usage: fornax chat <model> [-speak]")
	chatFlags.Parse(rest)
	if chatFlags.NArg() != 0 {
		return fmt.Errorf("usage: fornax chat <model> [-speak]")
	}
	var voice *catalog.Spec
	if *speak {
		voice = speechSpec(paths.Home())
		if voice == nil {
			return fmt.Errorf("-speak needs an installed speech model — `fornax pull hf:ggml-org/Qwen3-TTS-12Hz-1.7B-Base-GGUF`")
		}
	}
	return runChat(ctx, spec, eng, voice)
}

func cmdSee(ctx context.Context, args []string) error {
	spec, eng, rest, err := modelArgs(ctx, "see", args, " <image> [question…]")
	if err != nil {
		return err
	}
	if len(rest) < 1 {
		return fmt.Errorf("usage: fornax see <model> <image> [question…]")
	}
	question := strings.Join(rest[1:], " ")
	if question == "" {
		question = "Describe this image."
	}
	return runSee(ctx, spec, eng, rest[0], question)
}

func cmdHear(ctx context.Context, args []string) error {
	spec, eng, rest, err := modelArgs(ctx, "hear", args, " <audio> [question…]")
	if err != nil {
		return err
	}
	if len(rest) < 1 {
		return fmt.Errorf("usage: fornax hear <model> <audio> [question…]")
	}
	question := strings.Join(rest[1:], " ")
	if question == "" {
		question = "Transcribe what is said."
	}
	return runHear(ctx, spec, eng, rest[0], question)
}

func cmdTest(ctx context.Context, args []string) error {
	spec, eng, rest, err := modelArgs(ctx, "test", args, "")
	if err != nil {
		return err
	}
	if len(rest) > 0 {
		return fmt.Errorf("usage: fornax test <model>")
	}
	if spec.Kind == catalog.Decision {
		return runDecisionTest(ctx, spec)
	}
	if spec.Kind == catalog.Embed {
		return runEmbedTest(ctx, spec, eng)
	}
	if spec.Runtime == catalog.SD {
		does, command, operands := modelrt.Instead(spec)
		return fmt.Errorf("%s %s — time `fornax %s %s %s` instead", spec.ID, does, command, spec.ID, operands)
	}
	if spec.Kind == catalog.Speech {
		return runSayTest(ctx, spec, eng)
	}
	if spec.Kind == catalog.Rerank {
		return runRerankTest(ctx, spec, eng)
	}
	return runTest(ctx, spec, eng)
}

func cmdBench(ctx context.Context, args []string) error {
	spec, eng, rest, err := modelArgs(ctx, "bench", args, "")
	if err != nil {
		return err
	}
	if len(rest) > 0 {
		return fmt.Errorf("usage: fornax bench <model>")
	}
	if spec.Kind == catalog.Decision {
		return runDecisionBench(ctx, spec, 5)
	}
	if spec.Runtime == catalog.Apple {
		return runAppleBench(ctx, spec, 5)
	}
	if spec.Kind == catalog.Embed {
		return runEmbedBench(ctx, spec, eng, 5)
	}
	if does, command, _ := modelrt.Instead(spec); does != "" {
		return fmt.Errorf("%s %s — there is no bench for that; time `fornax %s %s …` instead", spec.ID, does, command, spec.ID)
	}
	return modelrt.Bench(ctx, spec, eng)
}

func cmdClean(args []string) error {
	set := flag.NewFlagSet("clean", flag.ExitOnError)
	all := set.Bool("all", false, "also remove every installed model and the engine")
	set.Usage = ui.UsageFunc(set, "usage: fornax clean [-all]  — removes partial downloads and stale staging")
	set.Parse(args)
	if set.NArg() > 0 {
		return fmt.Errorf("usage: fornax clean [-all]")
	}
	return modelrt.Clean(*all)
}

func runAppleBench(ctx context.Context, spec *catalog.Spec, calls int) error {
	return modelrt.WithServer(ctx, spec, nil, func(url, key string) error {
		var lat []float64
		for i := 0; i < calls; i++ {
			started := time.Now()
			if _, err := openai.Once(ctx, url, key, spec.ID,
				[]openai.Message{openai.TextMessage("user", "Reply with exactly: ok")}, 8); err != nil {
				return err
			}
			ms := float64(time.Since(started).Milliseconds())
			lat = append(lat, ms)
			fmt.Printf("  %s %.0f ms\n", ui.Dim(fmt.Sprintf("run %d", i+1)), ms)
		}
		sort.Float64s(lat)
		fmt.Printf("%s %s — median %.0f ms over %d requests\n", ui.Green("✓"), ui.Bold(spec.ID), lat[len(lat)/2], len(lat))
		return nil
	})
}
