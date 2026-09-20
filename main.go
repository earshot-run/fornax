// earshot-local — a workbench for local models: browse, download, run,
// talk to, see with, listen with, benchmark and clean up — on any machine,
// with or without Earshot.
//
// `earshot-local run qwen3-4b` fetches a pinned llama.cpp plus pinned
// weights, serves an OpenAI-compatible API on loopback behind a generated
// key, and hands the endpoint to a live Earshot daemon (or prints it to
// paste). `ask`, `chat`, `see` and `hear` use models directly.

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"
)

const usage = `earshot-local downloads pinned llama.cpp builds and pinned weights, then runs OpenAI-compatible model servers on loopback.

Fastest path: earshot-local run qwen3-4b

Usage: earshot-local <command>

Get models:
  list     Catalog: sizes, modality, fit on this machine, what is installed
  pull     Download a model (and the engine on first run); resumes if cut off
  rm       Delete a model's files and any partial download
  clean    Remove interrupted downloads and stale staging (-all wipes everything)
  doctor   What this machine can run; engine, keys and Earshot status

Use models:
  ask      One prompt, one answer (arg or stdin), streamed
  chat     Interactive conversation with history
  see      Ask a vision model about an image
  hear     Ask an audio model about a take — transcribes by default
  test     Load the model, run a prompt, report speed
  bench    llama-bench on the weights (pp/tg table)
  judge    Ask a kev decision model typed questions (TypeSafe API)

Run models:
  run      Serve a model on loopback; registers with Earshot. Ctrl-C stops
  ps       Which catalog models are serving right now
  connect  Register an already-running model's server with Earshot

Run "earshot-local <command> -h" for a command's flags.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch os.Args[1] {
	case "list":
		err = cmdList()
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
	case "run":
		err = cmdRun(ctx, os.Args[2:])
	case "ps":
		err = runPs()
	case "connect":
		err = cmdConnect(os.Args[2:])
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "earshot-local: unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	if err != nil {
		if errors.Is(err, context.Canceled) {
			fmt.Fprintln(os.Stderr, "earshot-local: interrupted")
			os.Exit(130)
		}
		fmt.Fprintf(os.Stderr, "earshot-local: %v\n", err)
		os.Exit(1)
	}
}

func cmdList() error {
	memory := memoryBytes()
	root := home()
	fmt.Printf("%-16s %-7s %-9s %-22s %-12s %s\n", "id", "kind", "size", "name", "fit", "status")
	for i := range models {
		spec := &models[i]
		var status string
		switch partial := modelPartialBytes(root, spec); {
		case modelInstalled(root, spec):
			status = "installed"
		case partial > 0:
			status = fmt.Sprintf("%d%% downloaded", partial*100/spec.totalBytes())
		default:
			status = "—"
		}
		fitLabel := modelFit(spec.sizeBytes(), memory).String()
		if modelFit(spec.sizeBytes(), memory) == fitWont {
			fitLabel = "needs " + humanSize(neededBytes(spec.sizeBytes())) + "+"
		}
		fmt.Printf("%-16s %-7s %-9s %-22s %-12s %s\n",
			spec.id, spec.kind, humanSize(spec.sizeBytes()), spec.name, fitLabel, status)
		fmt.Printf("%33s %s\n", "", spec.summary)
	}
	engineState := "unsupported platform"
	if engine() != nil {
		engineState = "supported"
	}
	fmt.Printf("\nthis machine: %s RAM · engine llama.cpp %s (%s)\n", humanSize(memory), engineVersion, engineState)
	return nil
}

func cmdPull(ctx context.Context, args []string) error {
	set := flag.NewFlagSet("pull", flag.ExitOnError)
	set.Usage = func() { fmt.Fprintln(os.Stderr, "usage: earshot-local pull <model>") }
	set.Parse(args)
	if set.NArg() != 1 {
		return fmt.Errorf("usage: earshot-local pull <model>")
	}
	spec, eng, err := resolve(set.Arg(0))
	if err != nil {
		return err
	}
	return pull(ctx, spec, eng)
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
	eng := engine()
	if eng == nil {
		return nil, nil, fmt.Errorf("earshot-local does not have a pinned llama.cpp for %s/%s yet", runtime.GOOS, runtime.GOARCH)
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
		fmt.Fprintf(os.Stderr, "engine llama.cpp %s installed\n", engineVersion)
	}
	if spec.rt == runtimeKev {
		bar := newProgress("kev runtime", kevSource.bytes)
		if err := ensureKevRuntime(ctx, root, bar.set); err != nil {
			return err
		}
	}
	if modelInstalled(root, spec) {
		fmt.Printf("%s already installed\n", spec.id)
		return nil
	}
	bar := newProgress(spec.id, spec.totalBytes())
	if err := ensureModel(ctx, root, spec, bar.set); err != nil {
		return err
	}
	fmt.Printf("%s installed (%s)\n", spec.id, humanSize(spec.totalBytes()))
	return nil
}

// The argument after the command is always the model id; everything after it
// is prompt text. Shared by ask/chat/see/hear/test/bench.
func modelArgs(cmd string, args []string, extra string) (*modelSpec, *engineSpec, []string, error) {
	set := flag.NewFlagSet(cmd, flag.ExitOnError)
	set.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: earshot-local %s <model>%s\n", cmd, extra)
	}
	set.Parse(args)
	if set.NArg() < 1 {
		return nil, nil, nil, fmt.Errorf("usage: earshot-local %s <model>%s", cmd, extra)
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
		return fmt.Errorf("%s answers typed questions, not prompts — use `earshot-local judge %s`", spec.id, spec.id)
	}
	prompt := strings.Join(rest, " ")
	if prompt == "" {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return err
		}
		prompt = strings.TrimSpace(string(data))
	}
	if prompt == "" {
		return fmt.Errorf("usage: earshot-local ask <model> <prompt…>")
	}
	return runAsk(ctx, spec, eng, prompt)
}

func cmdChat(ctx context.Context, args []string) error {
	spec, eng, _, err := modelArgs("chat", args, "")
	if err != nil {
		return err
	}
	if spec.rt == runtimeKev {
		return fmt.Errorf("%s answers typed questions, not chat — use `earshot-local judge %s`", spec.id, spec.id)
	}
	return runChat(ctx, spec, eng)
}

func cmdSee(ctx context.Context, args []string) error {
	spec, eng, rest, err := modelArgs("see", args, " <image> [question…]")
	if err != nil {
		return err
	}
	if len(rest) < 1 {
		return fmt.Errorf("usage: earshot-local see <model> <image> [question…]")
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
		return fmt.Errorf("usage: earshot-local hear <model> <audio> [question…]")
	}
	question := strings.Join(rest[1:], " ")
	if question == "" {
		question = "Transcribe what is said."
	}
	return runHear(ctx, spec, eng, rest[0], question)
}

func cmdTest(ctx context.Context, args []string) error {
	spec, eng, _, err := modelArgs("test", args, "")
	if err != nil {
		return err
	}
	if spec.rt == runtimeKev {
		return runKevTest(ctx, spec)
	}
	return runTest(ctx, spec, eng)
}

func cmdBench(ctx context.Context, args []string) error {
	spec, eng, _, err := modelArgs("bench", args, "")
	if err != nil {
		return err
	}
	if spec.rt == runtimeKev {
		return runKevBench(ctx, spec, 5)
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
		fmt.Fprintln(os.Stderr, `usage: earshot-local judge <model> --state "text" --ask 'id|type|instructions[|opts…]' [--ask …]
       earshot-local judge <model> --json request.json   ('-' reads stdin)

examples:
  earshot-local judge kev-4b --state "my order never arrived and I was charged twice" \
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
		return fmt.Errorf("usage: earshot-local judge <model> [--state …] [--ask …|--json …]")
	}
	spec, _, err := resolve(id)
	if err != nil {
		return err
	}
	if spec.rt != runtimeKev {
		return fmt.Errorf("%s chats, it does not judge — `judge` is for kev models", spec.id)
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
		fmt.Fprintln(os.Stderr, "usage: earshot-local clean [-all]  — removes partial downloads and stale staging")
	}
	set.Parse(args)
	return runClean(*all)
}

func cmdRun(ctx context.Context, args []string) error {
	set := flag.NewFlagSet("run", flag.ExitOnError)
	port := set.Int("port", 0, "loopback port to serve on (default: the model's catalog port)")
	ctxSize := set.Int("ctx-size", contextWindow, "context window passed to llama-server")
	noConnect := set.Bool("no-connect", false, "do not register the running server with Earshot")
	set.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: earshot-local run <model> [-port N] [-ctx-size N] [-no-connect]")
	}
	set.Parse(args)
	if set.NArg() != 1 {
		return fmt.Errorf("usage: earshot-local run <model>")
	}
	spec, eng, err := resolve(set.Arg(0))
	if err != nil {
		return err
	}
	servePort := spec.port
	if *port != 0 {
		servePort = *port
	}
	root := home()
	if err := pull(ctx, spec, eng); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "verifying %s…\n", spec.id)
	if err := rehash(root, spec); err != nil {
		return err
	}
	if err := portFree(servePort); err != nil {
		return err
	}
	var cmd *exec.Cmd
	var key string
	if spec.rt == runtimeKev {
		cmd, err = spawnKev(root, spec, servePort, "")
	} else {
		key, err = ensureKey(root)
		if err != nil {
			return err
		}
		cmd, err = spawnServer(root, eng, spec, servePort, *ctxSize, "")
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
	url := endpointURL(servePort)
	if spec.rt == runtimeKev {
		fmt.Printf("\n%s is serving the TypeSafe API at %s\n", spec.name, url)
		fmt.Println(kevBlock(url))
	} else {
		fmt.Printf("\n%s is serving at %s\n", spec.name, url)
		if *noConnect {
			fmt.Println(pasteBlock(url, key, spec.id))
		} else {
			reportConnect(url, key, spec.id)
		}
	}
	fmt.Println("\nctrl-c to stop")
	select {
	case <-ctx.Done():
		fmt.Fprintln(os.Stderr, "stopping…")
		killAndReap(cmd, exited)
		return nil
	case status := <-exited:
		return fmt.Errorf("llama-server exited: %v", status)
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
	set.Usage = func() { fmt.Fprintln(os.Stderr, "usage: earshot-local connect <model> [-port N]") }
	set.Parse(args)
	if set.NArg() != 1 {
		return fmt.Errorf("usage: earshot-local connect <model>")
	}
	spec := model(set.Arg(0))
	if spec == nil {
		return unknownModel(set.Arg(0))
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
		return fmt.Errorf("no server key yet — run `earshot-local run %s` once first", spec.id)
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
		fmt.Printf("earshot: connected — '%s' is in Settings ▸ Local models\n", spec.id)
		return nil
	case connectUnavailable:
		fmt.Printf("earshot: daemon answered but would not connect (%s)\n", detail)
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
	set.Usage = func() { fmt.Fprintln(os.Stderr, "usage: earshot-local rm <model>") }
	set.Parse(args)
	if set.NArg() != 1 {
		return fmt.Errorf("usage: earshot-local rm <model>")
	}
	spec := model(set.Arg(0))
	if spec == nil {
		return unknownModel(set.Arg(0))
	}
	dir := modelDir(home(), spec)
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		fmt.Printf("%s is not installed\n", spec.id)
		return nil
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("could not remove %s: %w", spec.name, err)
	}
	fmt.Printf("%s removed\n", spec.id)
	return nil
}

func cmdDoctor() error {
	root := home()
	fmt.Printf("home:     %s\n", root)
	fmt.Printf("platform: %s %s\n", runtime.GOOS, runtime.GOARCH)
	memory := memoryBytes()
	fmt.Printf("ram:      %s\n", humanSize(memory))
	if eng := engine(); eng != nil {
		state := "not downloaded"
		if engineInstalled(root, eng) {
			state = "installed"
		}
		fmt.Printf("engine:   llama.cpp %s (%s)\n", engineVersion, state)
	} else {
		fmt.Println("engine:   no pinned llama.cpp for this platform")
	}
	if runtime.GOOS == "windows" {
		fmt.Println("kev:      needs macOS or Linux")
	} else if _, err := exec.LookPath("uv"); err != nil {
		fmt.Println("kev:      `uv` not installed (needed to build its python env)")
	} else if kevRuntimeReady(root) {
		fmt.Println("kev:      runtime installed")
	} else {
		fmt.Println("kev:      runtime not built yet (first kev command builds it)")
	}
	cfg, cfgErr := loadConfig(root)
	key := "not yet"
	if cfgErr == nil && cfg.APIKey != "" {
		key = "generated"
	}
	fmt.Printf("key:      %s\n", key)
	for i := range models {
		spec := &models[i]
		if modelInstalled(root, spec) {
			serving := ""
			if cfgErr == nil && contains(servedModels(spec.port, cfg.APIKey), spec.id) {
				serving = ", serving"
			}
			fmt.Printf("model:    %s (%s) installed%s\n", spec.id, spec.kind, serving)
		}
	}
	if earshotPresent() {
		fmt.Println("earshot:  daemon config found")
	} else {
		fmt.Println("earshot:  not found on this computer")
	}
	return nil
}
