package main

// The workbench half: every command that *uses* a model goes through
// withServer — reuse the model's server if it is already running, otherwise
// pull, verify, spawn on a scratch port, use it, and reap it on the way out.

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/earshot-run/fornax/internal/catalog"
	"github.com/earshot-run/fornax/internal/openai"
	"github.com/earshot-run/fornax/internal/paths"
	"github.com/earshot-run/fornax/internal/ui"
)

// Ports 7431+ are scratch space for one-shot commands; each model's own port
// (7331+) belongs to `run` so a permanent server is never disturbed.
const scratchPortBase = 7431

func withServer(ctx context.Context, spec *catalog.Spec, eng *catalog.EngineSpec, fn func(url, key string) error) error {
	root := paths.Home()
	if err := pull(ctx, spec, eng); err != nil {
		return err
	}
	if spec.Runtime == catalog.Kev {
		return withKev(ctx, root, spec, fn)
	}
	if spec.Runtime == catalog.Laya {
		return withLaya(ctx, root, spec, fn)
	}
	if spec.Runtime == catalog.Apple {
		return withApple(ctx, root, spec, fn)
	}
	key, err := paths.EnsureKey(root)
	if err != nil {
		return err
	}
	if isServing(spec, key) {
		fmt.Fprintf(os.Stderr, "%s\n", ui.Dim(fmt.Sprintf("reusing %s on :%d", spec.ID, spec.Port)))
		return fn(paths.EndpointURL(spec.Port), key)
	}
	if err := rehash(root, spec); err != nil {
		return err
	}
	port, err := freePort(scratchPortBase)
	if err != nil {
		return err
	}
	logPath := filepath.Join(root, "server.log")
	cmd, err := spawnServer(root, eng, spec, port, catalog.ContextWindow, logPath)
	if err != nil {
		return err
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	defer killAndReap(cmd, exited)
	loading := ui.Spin("loading " + spec.ID)
	if err := waitReady(ctx, exited, port, spec.ID, key, true); err != nil {
		loading.Stop("")
		return fmt.Errorf("%w — server log: %s", err, logPath)
	}
	loading.Stop("")
	return fn(paths.EndpointURL(port), key)
}

// The kev path through withServer: python env first, then kev.serve.
// kev speaks /v1/systemone, not chat completions — no API key either.
func withKev(ctx context.Context, root string, spec *catalog.Spec, fn func(url, key string) error) error {
	if isServing(spec, "") {
		fmt.Fprintf(os.Stderr, "%s\n", ui.Dim(fmt.Sprintf("reusing %s on :%d", spec.ID, spec.Port)))
		return fn(paths.EndpointURL(spec.Port), "")
	}
	bar := ui.NewProgress("kev runtime", kevSource.Bytes)
	if err := ensureKevRuntime(ctx, root, bar.Set); err != nil {
		return err
	}
	if err := rehash(root, spec); err != nil {
		return err
	}
	port, err := freePort(scratchPortBase)
	if err != nil {
		return err
	}
	logPath := filepath.Join(root, "server.log")
	cmd, err := spawnKev(root, spec, port, logPath)
	if err != nil {
		return err
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	defer killAndReap(cmd, exited)
	loading := ui.Spin("loading " + spec.ID + " (first run downloads the base model)")
	if err := waitReady(ctx, exited, port, kevAlias, "", false); err != nil {
		loading.Stop("")
		return fmt.Errorf("%w — server log: %s", err, logPath)
	}
	loading.Stop("")
	return fn(paths.EndpointURL(port), "")
}

// The laya path through withServer: python env first, then the embedded
// serve shim. Unlike kev it answers behind the loopback key.
func withLaya(ctx context.Context, root string, spec *catalog.Spec, fn func(url, key string) error) error {
	key, err := paths.EnsureKey(root)
	if err != nil {
		return err
	}
	if isServing(spec, key) {
		fmt.Fprintf(os.Stderr, "%s\n", ui.Dim(fmt.Sprintf("reusing %s on :%d", spec.ID, spec.Port)))
		return fn(paths.EndpointURL(spec.Port), key)
	}
	bar := ui.NewProgress("laya runtime", layaSource.Bytes)
	if err := ensureLayaRuntime(ctx, root, bar.Set); err != nil {
		return err
	}
	if err := rehash(root, spec); err != nil {
		return err
	}
	port, err := freePort(scratchPortBase)
	if err != nil {
		return err
	}
	logPath := filepath.Join(root, "server.log")
	cmd, err := spawnLaya(root, spec, port, logPath)
	if err != nil {
		return err
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	defer killAndReap(cmd, exited)
	loading := ui.Spin("loading " + spec.ID)
	if err := waitReady(ctx, exited, port, spec.ID, key, true); err != nil {
		loading.Stop("")
		return fmt.Errorf("%w — server log: %s", err, logPath)
	}
	loading.Stop("")
	return fn(paths.EndpointURL(port), key)
}

// One prompt, one streamed reply. The prompt comes from the args or stdin.
func runAsk(ctx context.Context, spec *catalog.Spec, eng *catalog.EngineSpec, prompt string) error {
	return withServer(ctx, spec, eng, func(url, key string) error {
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
	return withServer(ctx, spec, eng, func(url, key string) error {
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
	return withServer(ctx, spec, eng, func(url, key string) error {
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
	return withServer(ctx, spec, eng, func(url, key string) error {
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
	return withServer(ctx, spec, eng, func(url, key string) error {
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

// The engine's own benchmark on the installed weights (pp512 / tg128 table).
func runBench(ctx context.Context, spec *catalog.Spec, eng *catalog.EngineSpec) error {
	root := paths.Home()
	if err := pull(ctx, spec, eng); err != nil {
		return err
	}
	if err := rehash(root, spec); err != nil {
		return err
	}
	bench := paths.EngineBinary(root, eng, eng.Bench)
	if _, err := os.Stat(bench); err != nil {
		return fmt.Errorf("llama-bench is not in the pinned engine")
	}
	fmt.Fprintf(os.Stderr, "%s\n", ui.Dim(fmt.Sprintf("llama-bench on %s (pp512 / tg128)", spec.ID)))
	cmd := exec.CommandContext(ctx, bench, "-m", paths.ModelFinal(root, spec))
	cmd.Dir = filepath.Dir(bench)
	cmd.Env = engineEnv(root, bench)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
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
	for _, spec := range allSpecs(root) {
		if isServing(spec, cfg.APIKey) {
			any = true
			fmt.Printf("%s %-16s %s :%d\n", ui.MarkOK(), spec.ID, ui.Dim("serving"), spec.Port)
		}
	}
	if !any {
		fmt.Println(ui.Dim("nothing is serving — `fornax run <model>` starts one"))
	}
	return nil
}

// Reclaim disk: interrupted downloads, stale staging, tmp writes.
// `-all` also removes every installed model and the engine.
func runClean(all bool) error {
	root := paths.Home()
	var freed int64
	var removed []string
	reap := func(path string) {
		info, err := os.Stat(path)
		if err != nil {
			return
		}
		if err := os.Remove(path); err == nil {
			freed += info.Size()
			removed = append(removed, path)
		}
	}
	filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			// Built runtimes are huge and never hold fornax's own .part files.
			switch path {
			case kevSrcDir(root), layaSrcDir(root):
				return filepath.SkipDir
			}
			if filepath.Dir(path) == paths.EnginesDir(root) {
				return filepath.SkipDir
			}
			return nil
		}
		name := filepath.Base(path)
		junk := strings.HasSuffix(name, ".part") || strings.HasSuffix(name, ".tmp-write") || name == "server.log"
		if !junk {
			return nil
		}
		// A file touched moments ago is probably a live download.
		if time.Since(info.ModTime()) < 10*time.Second {
			return nil
		}
		reap(path)
		return nil
	})
	// Staging dirs a crashed pull left: `.engine-*`/`.ckpt-*` at home root
	// and `src.staging` under each python runtime. A dir touched moments ago
	// is likely live.
	for _, parent := range []string{root, kevRoot(root), layaRoot(root)} {
		entries, err := os.ReadDir(parent)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			name := entry.Name()
			staging := entry.IsDir() &&
				(strings.HasPrefix(name, ".engine-") || strings.HasPrefix(name, ".ckpt-") || name == "src.staging")
			if !staging {
				continue
			}
			dir := filepath.Join(parent, name)
			if time.Since(dirFresh(dir)) < 10*time.Second {
				continue
			}
			freed += dirSize(dir)
			if os.RemoveAll(dir) == nil {
				removed = append(removed, dir+"/")
			}
		}
	}
	if all {
		for _, spec := range allSpecs(root) {
			dir := paths.ModelDir(root, spec)
			if _, err := os.Stat(dir); err == nil {
				freed += dirSize(dir)
				if os.RemoveAll(dir) == nil {
					removed = append(removed, dir+"/")
				}
			}
		}
		_, kevErr := os.Stat(kevRoot(root))
		for _, dir := range []string{filepath.Join(root, "engine"), kevRoot(root), layaRoot(root)} {
			freed += dirSize(dir)
			if _, err := os.Stat(dir); err != nil {
				continue
			}
			if err := os.RemoveAll(dir); err == nil {
				removed = append(removed, dir+"/")
			}
		}
		// The Qwen3 base lives in the shared HF cache — not ours to delete.
		if kevErr == nil {
			fmt.Println("note: the shared Hugging Face cache (~/.cache/huggingface) is left alone")
		}
		os.Remove(filepath.Join(root, customFile))
	}
	if len(removed) == 0 {
		fmt.Println(ui.Dim("nothing to clean"))
		return nil
	}
	for _, path := range removed {
		fmt.Printf("  %s %s\n", ui.Dim("−"), strings.TrimPrefix(path, root+string(os.PathSeparator)))
	}
	fmt.Printf("freed %s\n", ui.Bold(ui.HumanSize(freed)))
	return nil
}

// The newest mtime anywhere in a tree — a staging dir being actively
// unpacked keeps refreshing this.
func dirFresh(dir string) time.Time {
	fresh := time.Time{}
	filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err == nil && info.ModTime().After(fresh) {
			fresh = info.ModTime()
		}
		return nil
	})
	return fresh
}

func dirSize(dir string) int64 {
	var total int64
	filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	return total
}

// ask and chat both need a model that holds a conversation; every other kind
// gets pointed at the command that suits it.
func requireChat(spec *catalog.Spec) error {
	does, command, operands := instead(spec)
	if does == "" {
		return nil
	}
	return fmt.Errorf("%s %s, it does not chat — use `fornax %s %s %s`", spec.ID, does, command, spec.ID, operands)
}

func cmdAsk(ctx context.Context, args []string) error {
	spec, eng, rest, err := modelArgs(ctx, "ask", args, " [prompt…]  (or pipe it on stdin)")
	if err != nil {
		return err
	}
	if err := requireChat(spec); err != nil {
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
	if err := requireChat(spec); err != nil {
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
		does, command, operands := instead(spec)
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
	if does, command, _ := instead(spec); does != "" {
		return fmt.Errorf("%s %s — there is no bench for that; time `fornax %s %s …` instead", spec.ID, does, command, spec.ID)
	}
	return runBench(ctx, spec, eng)
}

func cmdClean(args []string) error {
	set := flag.NewFlagSet("clean", flag.ExitOnError)
	all := set.Bool("all", false, "also remove every installed model and the engine")
	set.Usage = ui.UsageFunc(set, "usage: fornax clean [-all]  — removes partial downloads and stale staging")
	set.Parse(args)
	if set.NArg() > 0 {
		return fmt.Errorf("usage: fornax clean [-all]")
	}
	return runClean(*all)
}
