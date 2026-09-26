// fornax — a workbench for local models: find, download, run,
// talk to, see with, listen with, benchmark and clean up — on any machine,
// with or without Earshot.
//
// `fornax run hf:Qwen/Qwen3-4B-GGUF` fetches a pinned llama.cpp plus pinned
// weights, serves an OpenAI-compatible API on loopback behind a generated
// key, and hands the endpoint to a live Earshot daemon (or prints it to
// paste). `ask`, `chat`, `see` and `hear` use models directly.

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/earshot-run/fornax/internal/mcp"
	"github.com/earshot-run/fornax/internal/ui"
)

const usage = `fornax downloads pinned llama.cpp builds and pinned weights, then runs OpenAI-compatible model servers on loopback.

Fastest path: fornax run hf:Qwen/Qwen3-4B-GGUF

Usage: fornax <command>

Get models:
  list       Your models: sizes, modality, fit on this machine, what is installed
  pull       Download a model — a saved id, hf:Org/Repo[/File.gguf], or ollama:<name>[:<tag>]
  rm         Delete a model's files and any partial download (-all: every model)
  clean      Remove interrupted downloads and stale staging (-all wipes everything)
  doctor     What this machine can run; engine, keys and Earshot status

Use models:
  ask        One prompt, one answer (arg or stdin), streamed
  chat       Interactive conversation with history
  see        Ask a vision model about an image
  hear       Ask an audio model about a take — transcribes by default
  test       Load the model, run a prompt, report speed
  bench      llama-bench on the weights (kev/laya/apple/embed: median request latency)
  judge      Ask a kev/laya decision model typed questions (TypeSafe API)
  embed      Turn text into a vector — JSON on stdout
  rerank     Score documents against a query, best first
  compare    Same prompt to several models, side by side
  imagine    Generate an image with a stable-diffusion.cpp model
  animate    Text or a still image to a short video clip
  studio     Image studio in the browser: queue, live progress, gallery, references
  say        Speak text with a speech model; -voice clones a reference take
  talk       Take in → transcribe → answer → spoken reply out
  record     Mic to WAV — the takes hear and talk consume

Run models:
  run        Serve a model on loopback; registers with Earshot. Ctrl-C stops
  ps         Which models are serving right now
  connect    Register an already-running model's server with Earshot

Workbench:
  show       Inspect a model's GGUF header — arch, params, quant, template
  search     Find GGUF repos on Hugging Face to pull
  pins       Audit your pinned models and engines against upstream; print re-pins for what moved
  version    Print the build version
  upgrade    Check for a newer fornax release
  mcp        Serve MCP on stdio — agents call ask/see/hear/embed/imagine/animate/say/models
  completion Shell completions: zsh, bash, fish

Run "fornax <command> -h" for a command's flags.
`

type command struct {
	name string
	run  func(context.Context, []string) error
}

// Every command fornax answers to. This table is the only list: dispatch,
// shell completion and the usage text are all checked against it. It is a
// function, not a var, because `completion` reads it and a var would make
// the package initialization cycle.
func commands() []command {
	return []command{
		{"list", func(_ context.Context, args []string) error { return cmdList(args) }},
		{"pull", cmdPull},
		{"rm", func(_ context.Context, args []string) error { return cmdRm(args) }},
		{"clean", func(_ context.Context, args []string) error { return cmdClean(args) }},
		{"doctor", func(_ context.Context, args []string) error { return cmdDoctor(args) }},
		{"ask", cmdAsk},
		{"chat", cmdChat},
		{"see", cmdSee},
		{"hear", cmdHear},
		{"test", cmdTest},
		{"bench", cmdBench},
		{"judge", cmdJudge},
		{"embed", cmdEmbed},
		{"rerank", cmdRerank},
		{"compare", cmdCompare},
		{"imagine", cmdImagine},
		{"animate", cmdAnimate},
		{"studio", cmdStudio},
		{"say", cmdSay},
		{"talk", cmdTalk},
		{"record", cmdRecord},
		{"run", cmdRun},
		{"ps", func(_ context.Context, args []string) error { return cmdPs(args) }},
		{"connect", func(_ context.Context, args []string) error { return cmdConnect(args) }},
		{"show", func(_ context.Context, args []string) error { return cmdShow(args) }},
		{"search", cmdSearch},
		{"pins", cmdPins},
		{"version", func(_ context.Context, args []string) error { return cmdVersion(args) }},
		{"upgrade", cmdUpgrade},
		{"mcp", cmdMCP},
		{"completion", func(_ context.Context, args []string) error { return cmdCompletion(args) }},
		{"__complete_models", func(context.Context, []string) error { return cmdCompleteModels() }},
	}
}

func cmdMCP(ctx context.Context, args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("usage: fornax mcp   (serves MCP on stdin/stdout)")
	}
	return mcp.Run(ctx, version)
}

func lookup(name string) func(context.Context, []string) error {
	for _, command := range commands() {
		if command.name == name {
			return command.run
		}
	}
	return nil
}

func main() {
	if len(os.Args) < 2 {
		ui.PrintUsage(os.Stderr, usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	name, args := os.Args[1], os.Args[2:]
	switch name {
	case "-h", "--help", "help":
		if len(args) == 0 {
			ui.PrintUsage(os.Stdout, usage)
			return
		}
		// `fornax help run` is `fornax run -h`.
		name, args = args[0], []string{"-h"}
	case "-v", "--version":
		name, args = "version", nil
	}
	run := lookup(name)
	if run == nil {
		fmt.Fprintf(os.Stderr, "%s unknown command %q%s\n\n", ui.Red("fornax:"), name, didYouMean(name))
		ui.PrintUsage(os.Stderr, usage)
		os.Exit(2)
	}
	if err := run(ctx, args); err != nil {
		if errors.Is(err, context.Canceled) {
			fmt.Fprintf(os.Stderr, "%s interrupted\n", ui.Dim("fornax:"))
			os.Exit(130)
		}
		fmt.Fprintf(os.Stderr, "%s %v\n", ui.Red("fornax:"), err)
		os.Exit(1)
	}
}

// A typo is the common case, so name the closest command when one is close
// enough to be worth guessing.
func didYouMean(name string) string {
	best, bestDistance := "", 0
	for _, command := range commands() {
		if strings.HasPrefix(command.name, "__") {
			continue
		}
		distance := editDistance(name, command.name)
		if best == "" || distance < bestDistance {
			best, bestDistance = command.name, distance
		}
	}
	if best == "" || bestDistance > 2 || bestDistance >= len(name) {
		return ""
	}
	return fmt.Sprintf(" — did you mean %s?", ui.Bold(best))
}

func editDistance(a, b string) int {
	previous := make([]int, len(b)+1)
	current := make([]int, len(b)+1)
	for j := range previous {
		previous[j] = j
	}
	for i := 1; i <= len(a); i++ {
		current[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			current[j] = min(previous[j]+1, current[j-1]+1, previous[j-1]+cost)
		}
		previous, current = current, previous
	}
	return previous[len(b)]
}
