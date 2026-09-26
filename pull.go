package main

// `fornax pull` — resolve an id (catalog, hf:, ollama:) and install it.

import (
	"context"
	"flag"
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/earshot-run/fornax/internal/catalog"
	"github.com/earshot-run/fornax/internal/engine"
	"github.com/earshot-run/fornax/internal/events"
	"github.com/earshot-run/fornax/internal/paths"
	"github.com/earshot-run/fornax/internal/ui"
)

func cmdPull(ctx context.Context, args []string) error {
	set := flag.NewFlagSet("pull", flag.ExitOnError)
	asEvents := set.Bool("events", false, "one JSON event per line on stdout")
	set.Usage = ui.UsageFunc(set, "usage: fornax pull [--events] <model>…   (a saved id, hf:Org/Repo[/File.gguf], or ollama:<name>[:<tag>])")
	// Pull flags like --as/--kind belong to the hf/ollama path; a plain id
	// or a bare Org/Repo shorthand resolves through the same machinery.
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") {
			continue
		}
		if isOllamaRef(arg) {
			return cmdPullOllama(ctx, args)
		}
		if isHFRef(arg) {
			return cmdPullHF(ctx, args)
		}
	}
	ids := parseFlexible(set, args, -1)
	if len(ids) < 1 {
		return fmt.Errorf("usage: fornax pull <model>…   (a saved id, hf:Org/Repo[/File.gguf], or ollama:<name>[:<tag>])")
	}
	if *asEvents {
		events.Enable()
	}
	for _, id := range ids {
		spec, eng, err := resolve(ctx, id)
		if err == nil {
			err = pull(ctx, spec, eng)
		}
		if err != nil {
			events.Emit("error", map[string]any{"model": id, "message": err.Error()})
			return err
		}
		events.Emit("installed", map[string]any{"model": spec.ID})
	}
	return nil
}

// An arg is a model reference when it carries an explicit scheme or is a
// bare Org/Repo — saved ids never contain a slash.
func isHFRef(arg string) bool {
	return strings.HasPrefix(arg, "hf:") || strings.HasPrefix(arg, "hf.co/") ||
		strings.Contains(arg, "huggingface.co/") || strings.Contains(arg, "/")
}

func isOllamaRef(arg string) bool {
	return strings.HasPrefix(arg, "ollama:") || strings.Contains(arg, "ollama.com/")
}

// resolve maps an arg to a model: a saved id, a built-in, or a fresh
// hf:/ollama: ref pinned and saved on the spot.
func resolve(ctx context.Context, arg string) (*catalog.Spec, *catalog.EngineSpec, error) {
	spec := model(arg)
	if spec == nil {
		var entry *customEntry
		var err error
		switch {
		case isOllamaRef(arg):
			entry, err = ensureOllama(ctx, arg, "", "")
		case isHFRef(arg):
			entry, err = ensureHF(ctx, arg, "", "", "", "", nil, nil)
		default:
			return nil, nil, unknownModel(arg)
		}
		if err != nil {
			return nil, nil, err
		}
		spec = entry.spec()
	}
	if spec.Runtime == catalog.Kev || spec.Runtime == catalog.Laya {
		if runtime.GOOS == "windows" {
			return nil, nil, fmt.Errorf("%s models need macOS or Linux (torch MPS/CUDA)", spec.Runtime)
		}
		return spec, nil, nil
	}
	if spec.Runtime == catalog.Apple {
		if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
			return nil, nil, fmt.Errorf("apple-fm is Apple's on-device model — it needs Apple Silicon on macOS 26+")
		}
		return spec, nil, nil
	}
	if spec.Runtime == catalog.SD {
		eng := engineSD()
		if eng == nil {
			return nil, nil, fmt.Errorf("fornax does not have a pinned stable-diffusion.cpp for %s/%s yet", runtime.GOOS, runtime.GOARCH)
		}
		return spec, eng, nil
	}
	eng, err := llamaEngine()
	if err != nil {
		return nil, nil, err
	}
	return spec, eng, nil
}

func llamaEngine() (*catalog.EngineSpec, error) {
	eng := catalog.Engine()
	if eng == nil {
		return nil, fmt.Errorf("fornax does not have a pinned llama.cpp for %s/%s yet", runtime.GOOS, runtime.GOARCH)
	}
	return eng, nil
}

func pull(ctx context.Context, spec *catalog.Spec, eng *catalog.EngineSpec) error {
	root := paths.Home()
	if err := paths.ProtectDir(root); err != nil {
		return err
	}
	if spec.Runtime == catalog.Llama && !paths.EngineInstalled(root, eng) {
		bar := ui.NewProgress("engine", eng.Bytes)
		if err := engine.Ensure(ctx, root, eng, bar.Set); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "%s engine llama.cpp %s installed\n", ui.Green("✓"), catalog.EngineVersion)
	}
	if spec.Runtime == catalog.Kev {
		bar := ui.NewProgress("kev runtime", kevSource.Bytes)
		if err := ensureKevRuntime(ctx, root, bar.Set); err != nil {
			return err
		}
	}
	if spec.Runtime == catalog.Laya {
		bar := ui.NewProgress("laya runtime", layaSource.Bytes)
		if err := ensureLayaRuntime(ctx, root, bar.Set); err != nil {
			return err
		}
	}
	if spec.Runtime == catalog.SD && !sdInstalled(root, eng) {
		bar := ui.NewProgress("sd engine", eng.Bytes)
		if err := ensureSDEngine(ctx, root, eng, bar.Set); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "%s engine stable-diffusion.cpp %s installed\n", ui.Green("✓"), sdVersion)
	}
	if modelInstalled(root, spec) {
		fmt.Fprintf(os.Stderr, "%s\n", ui.Dim(spec.ID+" already installed"))
		return nil
	}
	if spec.Runtime == catalog.Apple {
		if err := ensureModel(ctx, root, spec, nil); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "%s %s ready — the model itself ships in macOS\n", ui.Green("✓"), ui.Bold(spec.ID))
		return nil
	}
	bar := ui.NewProgress(spec.ID, spec.TotalBytes())
	if err := ensureModel(ctx, root, spec, bar.Set); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "%s %s installed (%s)\n", ui.Green("✓"), ui.Bold(spec.ID), ui.Dim(ui.HumanSize(spec.TotalBytes())))
	return nil
}

// Flags may lead or follow a command's positional arguments — `fornax pull
// hf:Org/Repo/File.gguf --as name` and `fornax pull --as name hf:…` both
// work — which flag.Parse alone does not allow: it stops at the first
// non-flag. n caps how many positionals may lead; n < 0 takes every one.
func parseFlexible(set *flag.FlagSet, args []string, n int) []string {
	var lead []string
	for (n < 0 || len(lead) < n) && len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		lead, args = append(lead, args[0]), args[1:]
	}
	set.Parse(args)
	return append(lead, set.Args()...)
}

// The argument after the command is always the model id; everything after it
// is prompt text. Shared by ask/chat/see/hear/test/bench.
func modelArgs(ctx context.Context, cmd string, args []string, extra string) (*catalog.Spec, *catalog.EngineSpec, []string, error) {
	set := flag.NewFlagSet(cmd, flag.ExitOnError)
	set.Usage = ui.UsageFunc(set, fmt.Sprintf("usage: fornax %s <model>%s", cmd, extra))
	set.Parse(args)
	if set.NArg() < 1 {
		return nil, nil, nil, fmt.Errorf("usage: fornax %s <model>%s", cmd, extra)
	}
	spec, eng, err := resolve(ctx, set.Arg(0))
	if err != nil {
		return nil, nil, nil, err
	}
	return spec, eng, set.Args()[1:], nil
}
