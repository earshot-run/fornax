// fornax — a workbench for local models: browse, download, run,
// talk to, see with, listen with, benchmark and clean up — on any machine,
// with or without Earshot.
//
// `fornax run qwen3-4b` fetches a pinned llama.cpp plus pinned
// weights, serves an OpenAI-compatible API on loopback behind a generated
// key, and hands the endpoint to a live Earshot daemon (or prints it to
// paste). `ask`, `chat`, `see` and `hear` use models directly.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
)

const usage = `fornax downloads pinned llama.cpp builds and pinned weights, then runs OpenAI-compatible model servers on loopback.

Fastest path: fornax run qwen3-4b

Usage: fornax <command>

Get models:
  list     Catalog: sizes, modality, fit on this machine, what is installed
  pull     Download a model — a catalog id, or hf:Org/Repo/File.gguf for any public GGUF
  rm       Delete a model's files and any partial download
  clean    Remove interrupted downloads and stale staging (-all wipes everything)
  doctor   What this machine can run; engine, keys and Earshot status

Use models:
  ask      One prompt, one answer (arg or stdin), streamed
  chat     Interactive conversation with history
  see      Ask a vision model about an image
  hear     Ask an audio model about a take — transcribes by default
  test     Load the model, run a prompt, report speed
  bench    llama-bench on the weights (kev/apple/embed: median request latency)
  judge    Ask a kev decision model typed questions (TypeSafe API)
  embed    Turn text into a vector — JSON on stdout
  rerank   Score documents against a query, best first
  compare  Same prompt to several models, side by side
  draw     Generate an image with a stable-diffusion.cpp model
  say      Speak text with a speech model; -voice clones a reference take
  talk     Take in → transcribe → answer → spoken reply out
  record   Mic to WAV — the takes hear and talk consume

Run models:
  run      Serve a model on loopback; registers with Earshot. Ctrl-C stops
  ps       Which catalog models are serving right now
  connect  Register an already-running model's server with Earshot

Workbench:
  show     Inspect a model's GGUF header — arch, params, quant, template
  search   Find GGUF repos on Hugging Face to pull
  version  Print the build version
  upgrade  Check for a newer fornax release
  mcp      Serve MCP on stdio — agents call ask/see/hear/embed/draw/say/list
  completion  Shell completions: zsh, bash, fish

Run "fornax <command> -h" for a command's flags.
`

func main() {
	if len(os.Args) < 2 {
		printUsage(os.Stderr)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch os.Args[1] {
	case "list":
		err = cmdList(os.Args[2:])
	case "pull":
		err = cmdPull(ctx, os.Args[2:])
	case "rm":
		err = cmdRm(os.Args[2:])
	case "clean":
		err = cmdClean(os.Args[2:])
	case "doctor":
		err = cmdDoctor()
	case "ask":
		err = cmdAsk(ctx, os.Args[2:])
	case "chat":
		err = cmdChat(ctx, os.Args[2:])
	case "see":
		err = cmdSee(ctx, os.Args[2:])
	case "hear":
		err = cmdHear(ctx, os.Args[2:])
	case "test":
		err = cmdTest(ctx, os.Args[2:])
	case "bench":
		err = cmdBench(ctx, os.Args[2:])
	case "judge":
		err = cmdJudge(ctx, os.Args[2:])
	case "embed":
		err = cmdEmbed(ctx, os.Args[2:])
	case "compare":
		err = cmdCompare(ctx, os.Args[2:])
	case "draw":
		err = cmdDraw(ctx, os.Args[2:])
	case "say":
		err = cmdSay(ctx, os.Args[2:])
	case "run":
		err = cmdRun(ctx, os.Args[2:])
	case "ps":
		err = runPs()
	case "connect":
		err = cmdConnect(os.Args[2:])
	case "rerank":
		err = cmdRerank(ctx, os.Args[2:])
	case "show":
		err = cmdShow(os.Args[2:])
	case "talk":
		err = cmdTalk(ctx, os.Args[2:])
	case "record":
		err = cmdRecord(ctx, os.Args[2:])
	case "mcp":
		err = cmdMCP(ctx, os.Args[2:])
	case "search":
		err = cmdSearch(ctx, os.Args[2:])
	case "version":
		err = cmdVersion(os.Args[2:])
	case "upgrade":
		err = cmdUpgrade(ctx, os.Args[2:])
	case "completion":
		err = cmdCompletion(os.Args[2:])
	case "__complete_models":
		err = cmdCompleteModels()
	case "-h", "--help", "help":
		printUsage(os.Stdout)
	default:
		fmt.Fprintf(os.Stderr, "%s unknown command %q\n\n", red("fornax:"), os.Args[1])
		printUsage(os.Stderr)
		os.Exit(2)
	}
	if err != nil {
		if errors.Is(err, context.Canceled) {
			fmt.Fprintf(os.Stderr, "%s interrupted\n", dim("fornax:"))
			os.Exit(130)
		}
		fmt.Fprintf(os.Stderr, "%s %v\n", red("fornax:"), err)
		os.Exit(1)
	}
}

// Pad to a visible width, then style — %-Ns would count the escape codes.
func cell(s string, width int, styleFn func(string) string) string {
	for len(s) < width {
		s += " "
	}
	if styleFn == nil {
		return s
	}
	return styleFn(s)
}

func fitLabel(spec *modelSpec, memory int64) (string, func(string) string) {
	if spec.rt == runtimeApple {
		if appleSupported() == nil {
			return "fits", green
		}
		return "needs macOS 26+", red
	}
	switch modelFit(spec.sizeBytes(), memory) {
	case fitFits:
		return "fits", green
	case fitTight:
		return "tight", yellow
	case fitWont:
		return "needs " + humanSize(neededBytes(spec.sizeBytes())) + "+", red
	}
	return "?", dim
}

func kindStyled(m modality) func(string) string {
	switch m {
	case modalVision:
		return magenta
	case modalAudio:
		return yellow
	case modalDecision:
		return green
	case modalImage:
		return blue
	case modalEmbed:
		return dim
	case modalSpeech:
		return pink
	case modalRerank:
		return dim
	}
	return cyan
}

func cmdList(args []string) error {
	set := flag.NewFlagSet("list", flag.ExitOnError)
	local := set.Bool("local", false, "installed models only")
	asJSON := set.Bool("json", false, "one JSON object per model on stdout")
	set.Usage = func() { fmt.Fprintln(os.Stderr, "usage: fornax list [--local] [--json]") }
	set.Parse(args)
	if set.NArg() != 0 {
		return fmt.Errorf("usage: fornax list [--local] [--json]")
	}
	memory := memoryBytes()
	root := home()
	specs := allSpecs(root)
	if *local {
		var kept []*modelSpec
		for _, spec := range specs {
			if modelInstalled(root, spec) {
				kept = append(kept, spec)
			}
		}
		specs = kept
	}
	if *asJSON {
		cfg, err := loadConfig(root)
		if err != nil {
			return err
		}
		serving := make([]bool, len(specs))
		var probes sync.WaitGroup
		for i, spec := range specs {
			probes.Add(1)
			go func(i int, spec *modelSpec) {
				defer probes.Done()
				alias, key := spec.id, cfg.APIKey
				if spec.rt == runtimeKev {
					alias, key = kevAlias, ""
				}
				serving[i] = contains(servedModels(spec.port, key), alias)
			}(i, spec)
		}
		probes.Wait()
		enc := json.NewEncoder(os.Stdout)
		for i, spec := range specs {
			fit, _ := fitLabel(spec, memory)
			enc.Encode(struct {
				ID        string `json:"id"`
				Name      string `json:"name"`
				Kind      string `json:"kind"`
				Runtime   string `json:"runtime"`
				Size      int64  `json:"bytes"`
				Partial   int64  `json:"partialBytes"`
				Fit       string `json:"fit"`
				Installed bool   `json:"installed"`
				Serving   bool   `json:"serving"`
				Port      int    `json:"port"`
				Repo      string `json:"repo"`
				Revision  string `json:"revision"`
				Summary   string `json:"summary"`
			}{spec.id, spec.name, spec.kind.String(), spec.rt.String(), spec.sizeBytes(), modelPartialBytes(root, spec), fit,
				modelInstalled(root, spec), serving[i], spec.port,
				spec.repo, spec.model.revision, spec.summary})
		}
		return nil
	}
	fmt.Printf("  %s %s %s %s %s\n",
		cell("id", 15, dim), cell("kind", 8, dim), cell("size", 7, dim), cell("fit", 12, dim), dim("status"))
	for _, spec := range specs {
		var status string
		switch partial := modelPartialBytes(root, spec); {
		case modelInstalled(root, spec):
			status = markOK() + " installed"
		case partial > 0:
			status = yellow("◐") + fmt.Sprintf(" %d%%", partial*100/spec.totalBytes())
		default:
			status = markIdle()
		}
		fit, fitStyle := fitLabel(spec, memory)
		size := humanSize(spec.sizeBytes())
		if spec.rt == runtimeApple {
			size = "os"
		}
		fmt.Printf("  %s %s %s %s %s\n",
			cell(spec.id, 15, bold),
			cell(spec.kind.String(), 8, kindStyled(spec.kind)),
			cell(size, 7, nil),
			cell(fit, 12, fitStyle),
			status)
		fmt.Printf("  %s %s\n", cell("", 15, nil), dim(spec.summary))
	}
	engineState := "unsupported platform"
	if engine() != nil {
		engineState = "supported"
	}
	fmt.Printf("\n  %s\n", dim(fmt.Sprintf("this machine: %s RAM · llama.cpp %s (%s)",
		humanSize(memory), engineVersion, engineState)))
	return nil
}

func cmdPull(ctx context.Context, args []string) error {
	for _, arg := range args {
		if strings.HasPrefix(arg, "hf:") || strings.Contains(arg, "huggingface.co/") {
			return cmdPullHF(ctx, args)
		}
		if strings.HasPrefix(arg, "ollama:") || strings.Contains(arg, "ollama.com/") {
			return cmdPullOllama(ctx, args)
		}
	}
	set := flag.NewFlagSet("pull", flag.ExitOnError)
	asEvents := set.Bool("events", false, "one JSON event per line on stdout")
	set.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: fornax pull [--events] <model>   (or hf:Org/Repo/File.gguf, ollama:<name>[:<tag>])")
	}
	set.Parse(args)
	if set.NArg() < 1 {
		return fmt.Errorf("usage: fornax pull <model>…")
	}
	if *asEvents {
		enableEvents()
	}
	for _, id := range set.Args() {
		spec, eng, err := resolve(id)
		if err == nil {
			err = pull(ctx, spec, eng)
		}
		if err != nil {
			emit("error", map[string]any{"model": id, "message": err.Error()})
			return err
		}
		emit("installed", map[string]any{"model": spec.id})
	}
	return nil
}

func resolve(id string) (*modelSpec, *engineSpec, error) {
	spec := model(id)
	if spec == nil {
		return nil, nil, unknownModel(id)
	}
	if spec.rt == runtimeKev {
		if runtime.GOOS == "windows" {
			return nil, nil, fmt.Errorf("kev models need macOS or Linux (torch MPS/CUDA)")
		}
		return spec, nil, nil
	}
	if spec.rt == runtimeApple {
		if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
			return nil, nil, fmt.Errorf("apple-fm is Apple's on-device model — it needs Apple Silicon on macOS 26+")
		}
		return spec, nil, nil
	}
	if spec.rt == runtimeSD {
		eng := engineSD()
		if eng == nil {
			return nil, nil, fmt.Errorf("fornax does not have a pinned stable-diffusion.cpp for %s/%s yet", runtime.GOOS, runtime.GOARCH)
		}
		return spec, eng, nil
	}
	eng := engine()
	if eng == nil {
		return nil, nil, fmt.Errorf("fornax does not have a pinned llama.cpp for %s/%s yet", runtime.GOOS, runtime.GOARCH)
	}
	return spec, eng, nil
}

func pull(ctx context.Context, spec *modelSpec, eng *engineSpec) error {
	root := home()
	if err := protectDir(root); err != nil {
		return err
	}
	if spec.rt == runtimeLlama && !engineInstalled(root, eng) {
		bar := newProgress("engine", eng.bytes)
		if err := ensureEngine(ctx, root, eng, bar.set); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "%s engine llama.cpp %s installed\n", green("✓"), engineVersion)
	}
	if spec.rt == runtimeKev {
		bar := newProgress("kev runtime", kevSource.bytes)
		if err := ensureKevRuntime(ctx, root, bar.set); err != nil {
			return err
		}
	}
	if spec.rt == runtimeSD && !sdInstalled(root, eng) {
		bar := newProgress("sd engine", eng.bytes)
		if err := ensureSDEngine(ctx, root, eng, bar.set); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "%s engine stable-diffusion.cpp %s installed\n", green("✓"), sdVersion)
	}
	if modelInstalled(root, spec) {
		fmt.Fprintf(os.Stderr, "%s\n", dim(spec.id+" already installed"))
		return nil
	}
	if spec.rt == runtimeApple {
		if err := ensureModel(ctx, root, spec, nil); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "%s %s ready — the model itself ships in macOS\n", green("✓"), bold(spec.id))
		return nil
	}
	bar := newProgress(spec.id, spec.totalBytes())
	if err := ensureModel(ctx, root, spec, bar.set); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "%s %s installed (%s)\n", green("✓"), bold(spec.id), dim(humanSize(spec.totalBytes())))
	return nil
}

// The argument after the command is always the model id; everything after it
// is prompt text. Shared by ask/chat/see/hear/test/bench.
func modelArgs(cmd string, args []string, extra string) (*modelSpec, *engineSpec, []string, error) {
	set := flag.NewFlagSet(cmd, flag.ExitOnError)
	set.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: fornax %s <model>%s\n", cmd, extra)
	}
	set.Parse(args)
	if set.NArg() < 1 {
		return nil, nil, nil, fmt.Errorf("usage: fornax %s <model>%s", cmd, extra)
	}
	spec, eng, err := resolve(set.Arg(0))
	if err != nil {
		return nil, nil, nil, err
	}
	return spec, eng, set.Args()[1:], nil
}

func cmdAsk(ctx context.Context, args []string) error {
	spec, eng, rest, err := modelArgs("ask", args, " [prompt…]  (or pipe it on stdin)")
	if err != nil {
		return err
	}
	if spec.rt == runtimeKev {
		return fmt.Errorf("%s answers typed questions, not prompts — use `fornax judge %s`", spec.id, spec.id)
	}
	if spec.kind == modalEmbed {
		return fmt.Errorf("%s embeds, it does not chat — use `fornax embed %s \"text\"`", spec.id, spec.id)
	}
	if spec.kind == modalImage {
		return fmt.Errorf("%s draws, it does not chat — use `fornax draw %s \"prompt\"`", spec.id, spec.id)
	}
	if spec.kind == modalSpeech {
		return fmt.Errorf("%s speaks, it does not chat — use `fornax say %s \"text\"`", spec.id, spec.id)
	}
	if spec.kind == modalRerank {
		return fmt.Errorf("%s ranks documents, it does not chat — use `fornax rerank %s \"query\" <doc…>`", spec.id, spec.id)
	}
	// --json / --schema sit after the model, before the prompt.
	structured := flag.NewFlagSet("ask", flag.ExitOnError)
	jsonOut := structured.Bool("json", false, "constrain the reply to a JSON object")
	schemaPath := structured.String("schema", "", "JSON Schema file to constrain the reply ('-' reads stdin)")
	structured.Usage = func() { fmt.Fprintln(os.Stderr, "usage: fornax ask <model> [--json|--schema f] [prompt…]") }
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
	spec, eng, rest, err := modelArgs("chat", args, "")
	if err != nil {
		return err
	}
	if spec.rt == runtimeKev {
		return fmt.Errorf("%s answers typed questions, not chat — use `fornax judge %s`", spec.id, spec.id)
	}
	if spec.kind == modalEmbed {
		return fmt.Errorf("%s embeds, it does not chat — use `fornax embed %s \"text\"`", spec.id, spec.id)
	}
	if spec.kind == modalImage {
		return fmt.Errorf("%s draws, it does not chat — use `fornax draw %s \"prompt\"`", spec.id, spec.id)
	}
	if spec.kind == modalSpeech {
		return fmt.Errorf("%s speaks, it does not chat — use `fornax say %s \"text\"`", spec.id, spec.id)
	}
	if spec.kind == modalRerank {
		return fmt.Errorf("%s ranks documents, it does not chat — use `fornax rerank %s \"query\" <doc…>`", spec.id, spec.id)
	}
	chatFlags := flag.NewFlagSet("chat", flag.ExitOnError)
	speak := chatFlags.Bool("speak", false, "read replies aloud through an installed speech model")
	chatFlags.Usage = func() { fmt.Fprintln(os.Stderr, "usage: fornax chat <model> [-speak]") }
	chatFlags.Parse(rest)
	if chatFlags.NArg() != 0 {
		return fmt.Errorf("usage: fornax chat <model> [-speak]")
	}
	var voice *modelSpec
	if *speak {
		voice = speechSpec(home())
		if voice == nil {
			return fmt.Errorf("-speak needs an installed speech model — `fornax pull qwen3-tts-1.7b`")
		}
	}
	return runChat(ctx, spec, eng, voice)
}

func cmdSee(ctx context.Context, args []string) error {
	spec, eng, rest, err := modelArgs("see", args, " <image> [question…]")
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
	spec, eng, rest, err := modelArgs("hear", args, " <audio> [question…]")
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
	spec, eng, rest, err := modelArgs("test", args, "")
	if err != nil {
		return err
	}
	if len(rest) > 0 {
		return fmt.Errorf("usage: fornax test <model>")
	}
	if spec.rt == runtimeKev {
		return runKevTest(ctx, spec)
	}
	if spec.kind == modalEmbed {
		return runEmbedTest(ctx, spec, eng)
	}
	if spec.kind == modalImage {
		return fmt.Errorf("%s draws, it does not chat — `fornax draw %s \"prompt\"`", spec.id, spec.id)
	}
	if spec.kind == modalSpeech {
		return runSayTest(ctx, spec, eng)
	}
	if spec.kind == modalRerank {
		return runRerankTest(ctx, spec, eng)
	}
	return runTest(ctx, spec, eng)
}

func cmdBench(ctx context.Context, args []string) error {
	spec, eng, rest, err := modelArgs("bench", args, "")
	if err != nil {
		return err
	}
	if len(rest) > 0 {
		return fmt.Errorf("usage: fornax bench <model>")
	}
	if spec.rt == runtimeKev {
		return runKevBench(ctx, spec, 5)
	}
	if spec.rt == runtimeApple {
		return runAppleBench(ctx, spec, 5)
	}
	if spec.kind == modalEmbed {
		return runEmbedBench(ctx, spec, eng, 5)
	}
	if spec.kind == modalImage {
		return fmt.Errorf("%s draws — there is no bench for image models; time `fornax draw %s …` instead", spec.id, spec.id)
	}
	if spec.kind == modalSpeech {
		return fmt.Errorf("%s speaks — there is no bench for speech models; time `fornax say %s …` instead", spec.id, spec.id)
	}
	if spec.kind == modalRerank {
		return fmt.Errorf("%s ranks — there is no bench for rerank models; time `fornax rerank %s …` instead", spec.id, spec.id)
	}
	return runBench(ctx, spec, eng)
}

// Repeatable --ask flag for `judge`.
type askFlags []string

func (f *askFlags) String() string { return "" }
func (f *askFlags) Set(v string) error {
	*f = append(*f, v)
	return nil
}

func cmdJudge(ctx context.Context, args []string) error {
	set := flag.NewFlagSet("judge", flag.ExitOnError)
	state := set.String("state", "", "the document the questions judge")
	jsonPath := set.String("json", "", "a raw SystemOne request file ('-' reads stdin)")
	var asks askFlags
	set.Var(&asks, "ask", "typed question: 'id|noul|instructions' or 'id|choice|instructions|opt1|opt2…' (repeatable)")
	set.Usage = func() {
		fmt.Fprintln(os.Stderr, `usage: fornax judge <model> --state "text" --ask 'id|type|instructions[|opts…]' [--ask …]
       fornax judge <model> --json request.json   ('-' reads stdin)

examples:
  fornax judge kev-4b --state "my order never arrived and I was charged twice" \
    --ask 'escalate|noul|Needs urgent human attention?' \
    --ask 'team|choice|Which team?|returns|shipping|billing' \
    --ask 'mood|score|How upset?|calm|frustrated|angry'`)
	}
	// `judge <model> --flags…` reads naturally; flag.Parse needs flags first.
	var id string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		id, args = args[0], args[1:]
	}
	set.Parse(args)
	if id == "" && set.NArg() > 0 {
		id = set.Arg(0)
	}
	if id == "" {
		return fmt.Errorf("usage: fornax judge <model> [--state …] [--ask …|--json …]")
	}
	spec, _, err := resolve(id)
	if err != nil {
		return err
	}
	if spec.rt != runtimeKev {
		return fmt.Errorf("%s chats, it does not judge — `judge` is for kev models", spec.id)
	}
	if *jsonPath != "" && (*state != "" || len(asks) > 0) {
		return fmt.Errorf("--json is a complete request body — drop --state and --ask")
	}
	if *jsonPath == "" && *state == "" {
		return fmt.Errorf("nothing to judge — pass --state \"text\" or --json request.json")
	}
	return runJudge(ctx, spec, *state, asks, *jsonPath)
}

func cmdClean(args []string) error {
	set := flag.NewFlagSet("clean", flag.ExitOnError)
	all := set.Bool("all", false, "also remove every installed model and the engine")
	set.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: fornax clean [-all]  — removes partial downloads and stale staging")
	}
	set.Parse(args)
	if set.NArg() > 0 {
		return fmt.Errorf("usage: fornax clean [-all]")
	}
	return runClean(*all)
}

func cmdRun(ctx context.Context, args []string) error {
	set := flag.NewFlagSet("run", flag.ExitOnError)
	port := set.Int("port", 0, "loopback port to serve on (default: the model's catalog port)")
	ctxSize := set.Int("ctx-size", contextWindow, "context window passed to llama-server")
	noConnect := set.Bool("no-connect", false, "do not register the running server with Earshot")
	idle := set.Duration("idle", 0, "stop after this long without a request, e.g. 20m (default: never)")
	asEvents := set.Bool("events", false, "one JSON event per line on stdout; stops when the reader goes away")
	set.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: fornax run <model> [-port N] [-ctx-size N] [-idle 20m] [-no-connect] [--events]")
	}
	// `run <model> -flags` reads naturally; flag.Parse needs flags first.
	var id string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		id, args = args[0], args[1:]
	}
	set.Parse(args)
	if id == "" {
		if set.NArg() == 1 {
			id = set.Arg(0)
		} else {
			return fmt.Errorf("usage: fornax run <model> [-port N] [-ctx-size N] [-idle 20m] [-no-connect] [--events]")
		}
	} else if set.NArg() > 0 {
		return fmt.Errorf("usage: fornax run <model> [-port N] [-ctx-size N] [-idle 20m] [-no-connect] [--events]")
	}
	if *asEvents {
		enableEvents()
	}
	err := serveModel(ctx, id, *port, *ctxSize, *noConnect, *idle)
	if err != nil {
		emit("error", map[string]any{"message": err.Error()})
	}
	return err
}

func serveModel(ctx context.Context, id string, port, ctxSize int, noConnect bool, idle time.Duration) error {
	spec, eng, err := resolve(id)
	if err != nil {
		return err
	}
	if spec.rt == runtimeSD {
		return fmt.Errorf("%s draws, it does not serve — `fornax draw %s \"prompt\"`", spec.id, spec.id)
	}
	if spec.kind == modalSpeech {
		return fmt.Errorf("%s speaks, it does not serve — `fornax say %s \"text\"`", spec.id, spec.id)
	}
	servePort := spec.port
	if port != 0 {
		servePort = port
	}
	root := home()
	if err := pull(ctx, spec, eng); err != nil {
		return err
	}
	if spec.rt == runtimeApple {
		return runApple(ctx, root, spec, servePort, ctxSize != contextWindow, noConnect, idle)
	}
	emit("stage", map[string]any{"stage": "verifying", "model": spec.id})
	verifying := spin("verifying " + spec.id)
	if err := rehash(root, spec); err != nil {
		verifying.stop("")
		return err
	}
	verifying.stop("")
	if err := portFree(servePort); err != nil {
		return err
	}
	var cmd *exec.Cmd
	var key string
	serverName := "llama-server"
	logPath := ""
	if events != nil {
		logPath = serverLog(root)
	}
	emit("stage", map[string]any{"stage": "loading", "model": spec.id})
	if spec.rt == runtimeKev {
		serverName = "kev.serve"
		if ctxSize != contextWindow {
			fmt.Fprintln(os.Stderr, "note: -ctx-size is ignored for kev models")
		}
		if noConnect {
			fmt.Fprintln(os.Stderr, "note: -no-connect is ignored for kev models (no Earshot route)")
		}
		if idle > 0 {
			fmt.Fprintln(os.Stderr, "note: -idle is ignored for kev models (no activity counters)")
			idle = 0
		}
		cmd, err = spawnKev(root, spec, servePort, logPath)
	} else {
		key, err = ensureKey(root)
		if err != nil {
			return err
		}
		cmd, err = spawnServer(root, eng, spec, servePort, ctxSize, logPath)
	}
	if err != nil {
		return err
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	if spec.rt == runtimeKev {
		err = waitReady(ctx, exited, servePort, kevAlias, "", false)
	} else {
		err = waitReady(ctx, exited, servePort, spec.id, key, true)
	}
	if err != nil {
		killAndReap(cmd, exited)
		return err
	}
	probe := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{Proxy: nil}}
	return holdServing(ctx, &serving{
		spec: spec, port: servePort, key: key, noConnect: noConnect, idle: idle,
		process: serverName, exited: exited, stop: func() { killAndReap(cmd, exited) },
		sample: func() (string, bool) { return metricsFingerprint(probe, servePort, key) },
	})
}

// A model that is up: announce it, then hold until something ends it.
type serving struct {
	spec      *modelSpec
	port      int
	key       string
	noConnect bool
	idle      time.Duration
	process   string
	exited    <-chan error
	stop      func()
	sample    func() (string, bool)
}

func holdServing(ctx context.Context, s *serving) error {
	url := endpointURL(s.port)
	if events != nil {
		earshot := "skipped"
		if s.spec.rt != runtimeKev && !s.noConnect {
			switch result, _ := connectEarshot(url, s.key); result {
			case connectRegistered:
				earshot = "connected"
			case connectUnavailable:
				earshot = "refused"
			default:
				earshot = "absent"
			}
		}
		emit("ready", map[string]any{"model": s.spec.id, "url": url, "port": s.port, "earshot": earshot})
	} else {
		fmt.Printf("\n%s %s\n", markOK(), bold(s.spec.name)+" is serving")
		fmt.Printf("    %s %s\n", dim("url:"), cyan(url))
		switch {
		case s.spec.rt == runtimeKev:
			fmt.Println(kevBlock(url))
		case s.noConnect:
			fmt.Println(pasteBlock(url, s.key, s.spec.id))
		default:
			reportConnect(url, s.key, s.spec.id)
		}
		if s.idle > 0 {
			fmt.Println(dim(fmt.Sprintf("\nstops after %s without a request · ctrl-c to stop now", s.idle)))
		} else {
			fmt.Println(dim("\nctrl-c to stop"))
		}
	}
	done := make(chan struct{})
	defer close(done)
	go heartbeat(done)
	select {
	case <-ctx.Done():
		fmt.Fprintln(os.Stderr, dim("stopping…"))
		s.stop()
		emit("stopped", map[string]any{"model": s.spec.id, "reason": "signal"})
		return nil
	case <-supervisorGone():
		s.stop()
		return nil
	case <-idleAfter(s.idle, idlePoll, s.sample, done):
		fmt.Fprintln(os.Stderr, dim(fmt.Sprintf("no requests for %s — stopping", s.idle)))
		s.stop()
		emit("stopped", map[string]any{"model": s.spec.id, "reason": "idle"})
		return nil
	case status := <-s.exited:
		s.stop()
		return fmt.Errorf("%s exited: %v", s.process, status)
	}
}

func killAndReap(cmd *exec.Cmd, exited <-chan error) {
	cmd.Process.Kill()
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
	}
}

func cmdConnect(args []string) error {
	set := flag.NewFlagSet("connect", flag.ExitOnError)
	port := set.Int("port", 0, "loopback port the model is served on (default: the model's catalog port)")
	set.Usage = func() { fmt.Fprintln(os.Stderr, "usage: fornax connect <model> [-port N]") }
	var id string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		id, args = args[0], args[1:]
	}
	set.Parse(args)
	if id == "" {
		if set.NArg() == 1 {
			id = set.Arg(0)
		} else {
			return fmt.Errorf("usage: fornax connect <model> [-port N]")
		}
	} else if set.NArg() > 0 {
		return fmt.Errorf("usage: fornax connect <model> [-port N]")
	}
	spec := model(id)
	if spec == nil {
		return unknownModel(id)
	}
	if spec.rt == runtimeSD {
		return fmt.Errorf("%s draws, it does not serve — `fornax draw %s \"prompt\"`", spec.id, spec.id)
	}
	if spec.kind == modalSpeech {
		return fmt.Errorf("%s speaks, it does not serve — `fornax say %s \"text\"`", spec.id, spec.id)
	}
	servePort := spec.port
	if *port != 0 {
		servePort = *port
	}
	if spec.rt == runtimeKev {
		if contains(servedModels(servePort, ""), kevAlias) {
			fmt.Println(kevBlock(endpointURL(servePort)))
			return nil
		}
		return fmt.Errorf("nothing is answering on 127.0.0.1:%d — is `%s` running?", servePort, spec.id)
	}
	root := home()
	cfg, err := loadConfig(root)
	if err != nil {
		return err
	}
	if cfg.APIKey == "" {
		return fmt.Errorf("no server key yet — run `fornax run %s` once first", spec.id)
	}
	served := servedModels(servePort, cfg.APIKey)
	switch {
	case served == nil:
		return fmt.Errorf("nothing is answering on 127.0.0.1:%d — is `%s` running?", servePort, spec.id)
	case !contains(served, spec.id):
		return fmt.Errorf("127.0.0.1:%d serves [%s], not %s", servePort, strings.Join(served, ", "), spec.id)
	}
	url := endpointURL(servePort)
	result, detail := connectEarshot(url, cfg.APIKey)
	switch result {
	case connectRegistered:
		fmt.Printf("%s %s connected — %s is in Settings ▸ Local models\n", markOK(), green("earshot:"), bold(spec.id))
		return nil
	case connectUnavailable:
		fmt.Printf("%s daemon would not connect (%s)\n", yellow("earshot:"), detail)
		fmt.Println(pasteBlock(url, cfg.APIKey, spec.id))
		return nil
	default:
		return fmt.Errorf("no Earshot daemon on this computer\n%s", pasteBlock(url, cfg.APIKey, spec.id))
	}
}

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

func cmdRm(args []string) error {
	set := flag.NewFlagSet("rm", flag.ExitOnError)
	set.Usage = func() { fmt.Fprintln(os.Stderr, "usage: fornax rm <model>") }
	set.Parse(args)
	if set.NArg() != 1 {
		return fmt.Errorf("usage: fornax rm <model>")
	}
	spec := model(set.Arg(0))
	if spec == nil {
		return unknownModel(set.Arg(0))
	}
	dir := modelDir(home(), spec)
	custom := customSpec(home(), spec.id) != nil
	if _, err := os.Stat(dir); os.IsNotExist(err) && !custom {
		fmt.Printf("%s %s is not installed\n", markIdle(), spec.id)
		return nil
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("could not remove %s: %w", spec.name, err)
	}
	if custom {
		if err := dropCustom(home(), spec.id); err != nil {
			return fmt.Errorf("removed files but could not update %s: %w", customFile, err)
		}
	}
	fmt.Printf("%s %s removed\n", green("✓"), spec.id)
	if spec.rt == runtimeKev {
		fmt.Println(dim("note: the shared Hugging Face cache (~/.cache/huggingface) is left alone"))
	}
	return nil
}

func cmdDoctor() error {
	root := home()
	row := func(mark, label, value string) {
		fmt.Printf("  %s %s %s\n", mark, cell(label, 8, dim), value)
	}
	row(" ", "fornax", version)
	row(" ", "home", root)
	row(" ", "machine", fmt.Sprintf("%s %s · %s RAM", runtime.GOOS, runtime.GOARCH, humanSize(memoryBytes())))
	if eng := engine(); eng != nil {
		if engineInstalled(root, eng) {
			row(markOK(), "engine", "llama.cpp "+engineVersion)
		} else {
			row(markIdle(), "engine", "llama.cpp "+engineVersion+dim(" — first pull downloads it"))
		}
	} else {
		row(red("✗"), "engine", "no pinned llama.cpp for this platform")
	}
	switch {
	case runtime.GOOS == "windows":
		row(markIdle(), "kev", "needs macOS or Linux")
	default:
		if _, err := exec.LookPath("uv"); err != nil {
			row(red("✗"), "kev", "needs `uv` — https://docs.astral.sh/uv/")
		} else if kevRuntimeReady(root) {
			row(markOK(), "kev", "runtime installed")
		} else {
			row(markIdle(), "kev", dim("runtime not built yet — first kev command builds it"))
		}
	}
	if appleSpec := model("apple-fm"); appleSpec != nil {
		switch err := appleSupported(); {
		case err != nil:
			row(markIdle(), "apple-fm", dim("unsupported here — needs Apple Silicon on macOS 26+"))
		case appleInstalled(root, appleSpec):
			row(markOK(), "apple-fm", "bridge compiled — ready to serve")
		default:
			row(markIdle(), "apple-fm", dim("supported — `fornax pull apple-fm` compiles the bridge"))
		}
	}
	cfg, cfgErr := loadConfig(root)
	if cfgErr == nil && cfg.APIKey != "" {
		row(markOK(), "key", "generated")
	} else {
		row(markIdle(), "key", dim("generated on first run"))
	}
	for _, spec := range allSpecs(root) {
		if modelInstalled(root, spec) {
			alias, key := spec.id, cfg.APIKey
			if spec.rt == runtimeKev {
				alias, key = kevAlias, ""
			}
			serving := ""
			if cfgErr == nil && contains(servedModels(spec.port, key), alias) {
				serving = green(fmt.Sprintf(" — serving :%d", spec.port))
			}
			row(markOK(), "model", fmt.Sprintf("%s %s%s", spec.id, dim("("+spec.kind.String()+")"), serving))
		}
	}
	if earshotPresent() {
		row(markOK(), "earshot", "daemon config found")
	} else {
		row(markIdle(), "earshot", dim("not found on this computer"))
	}
	return nil
}
