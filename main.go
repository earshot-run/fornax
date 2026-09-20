// earshot-local — browse, download, and run local models for Earshot.
//
// `earshot-local run qwen3-4b` is the whole job: fetch a pinned llama.cpp,
// fetch a pinned GGUF, serve an OpenAI-compatible API on loopback with a
// generated key, and hand the URL to a live Earshot daemon (or print it to
// paste).

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"
)

const usage = `earshot-local downloads pinned llama.cpp builds and pinned GGUF weights, then runs an OpenAI-compatible server on loopback that Earshot can use.

Fastest path: earshot-local run qwen3-4b

Usage: earshot-local <command>

Commands:
  list     Show the catalog: sizes, fit on this machine, what is installed
  pull     Download a model (and the engine on first run); resumes interrupted downloads
  run      Serve a model on loopback, then register it with Earshot; Ctrl-C stops
  connect  Register an already-running model's server with Earshot
  rm       Delete a model's weights and any partial download
  doctor   What this machine can run and how Earshot connectivity looks

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
	case "run":
		err = cmdRun(ctx, os.Args[2:])
	case "connect":
		err = cmdConnect(os.Args[2:])
	case "rm":
		err = cmdRm(os.Args[2:])
	case "doctor":
		err = cmdDoctor()
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
	fmt.Printf("%-14s %-10s %-22s %-12s %s\n", "id", "size", "name", "fit", "status")
	root := home()
	for i := range models {
		spec := &models[i]
		var status string
		switch partial := partialBytes(modelPart(root, spec), spec.bytes); {
		case modelInstalled(root, spec):
			status = "installed"
		case partial > 0:
			status = fmt.Sprintf("%d%% downloaded", partial*100/spec.bytes)
		default:
			status = "—"
		}
		fitLabel := modelFit(spec.bytes, memory).String()
		if modelFit(spec.bytes, memory) == fitWont {
			fitLabel = "needs " + humanSize(neededBytes(spec.bytes)) + "+"
		}
		fmt.Printf("%-14s %-10s %-22s %-12s %s\n", spec.id, humanSize(spec.bytes), spec.name, fitLabel, status)
		fmt.Printf("%24s %s\n", "", spec.summary)
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
	if !engineInstalled(root, eng) {
		bar := newProgress("engine", eng.bytes)
		if err := ensureEngine(ctx, root, eng, bar.set); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "engine llama.cpp %s installed\n", engineVersion)
	}
	if modelInstalled(root, spec) {
		fmt.Printf("%s already installed\n", spec.id)
		return nil
	}
	bar := newProgress(spec.id, spec.bytes)
	if err := ensureModel(ctx, root, spec, bar.set); err != nil {
		return err
	}
	fmt.Printf("%s installed (%s)\n", spec.id, humanSize(spec.bytes))
	return nil
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
	key, err := ensureKey(root)
	if err != nil {
		return err
	}
	if err := portFree(servePort); err != nil {
		return err
	}
	cmd, err := spawnServer(root, eng, spec, servePort, *ctxSize)
	if err != nil {
		return err
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	if err := waitReady(ctx, cmd, exited, servePort, spec.id, key); err != nil {
		killAndReap(cmd, exited)
		return err
	}
	url := endpointURL(servePort)
	fmt.Printf("\n%s is serving at %s\n", spec.name, url)
	if *noConnect {
		fmt.Println(pasteBlock(url, key, spec.id))
	} else {
		reportConnect(url, key, spec.id)
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
	root := home()
	cfg, err := loadConfig(root)
	if err != nil {
		return err
	}
	if cfg.APIKey == "" {
		return fmt.Errorf("no server key yet — run `earshot-local run %s` once first", spec.id)
	}
	servePort := spec.port
	if *port != 0 {
		servePort = *port
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
			fmt.Printf("model:    %s installed%s\n", spec.id, serving)
		}
	}
	if earshotPresent() {
		fmt.Println("earshot:  daemon config found")
	} else {
		fmt.Println("earshot:  not found on this computer")
	}
	return nil
}
