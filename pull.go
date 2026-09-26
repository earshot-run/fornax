package main

// `fornax pull` — resolve an id (catalog, hf:, ollama:) and install it.

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/earshot-run/fornax/internal/catalog"
	"github.com/earshot-run/fornax/internal/events"
	"github.com/earshot-run/fornax/internal/modelrt"
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
		if modelrt.IsOllamaRef(arg) {
			return cmdPullOllama(ctx, args)
		}
		if modelrt.IsHFRef(arg) {
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
		spec, eng, err := modelrt.Resolve(ctx, id)
		if err == nil {
			err = modelrt.Pull(ctx, spec, eng)
		}
		if err != nil {
			events.Emit("error", map[string]any{"model": id, "message": err.Error()})
			return err
		}
		events.Emit("installed", map[string]any{"model": spec.ID})
	}
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
	spec, eng, err := modelrt.Resolve(ctx, set.Arg(0))
	if err != nil {
		return nil, nil, nil, err
	}
	return spec, eng, set.Args()[1:], nil
}

// `fornax pull hf:Org/Repo[/File.gguf] [--as name] [--kind …]`, or paste a
// huggingface.co URL. No file picks a sensible quant; split archives pull
// every part; a repo mmproj attaches itself when the kind wants one.
func cmdPullHF(ctx context.Context, args []string) error {
	set := flag.NewFlagSet("pull", flag.ExitOnError)
	as := set.String("as", "", "custom id to save the model under")
	kind := set.String("kind", "", "text | vision | audio | image | video | embed | rerank | speech (default: infer)")
	var with companionFlags
	set.Var(&with, "with", "image/video: a file sd-cli loads beside the weights, as <sd-cli flag>=hf:Org/Repo/File (repeatable)")
	sdArgs := set.String("args", "", "image/video: sd-cli arguments saved with the model")
	mmproj := set.String("mmproj", "", "projector file in the same repo (default: auto-detect for vision/audio/speech)")
	rev := set.String("rev", "", "branch, tag or commit (default: main)")
	asEvents := set.Bool("events", false, "one JSON event per line on stdout")
	set.Usage = ui.UsageFunc(set, `usage: fornax pull hf:Org/Repo[/File.gguf] [--as name] [--kind K] [--mmproj F] [--events]
       (or paste a huggingface.co repo/blob/resolve URL)`)
	ref := parseFlexible(set, args, 1)
	if len(ref) != 1 {
		return fmt.Errorf("usage: fornax pull hf:Org/Repo[/File.gguf] [--as name] [--kind K]")
	}
	if *asEvents {
		events.Enable()
	}
	spec, err := func() (*catalog.Spec, error) {
		spec, err := modelrt.EnsureHF(ctx, ref[0], modelrt.HFOptions{
			As: *as, Kind: *kind, MMProj: *mmproj, Rev: *rev, With: with, Args: strings.Fields(*sdArgs),
		})
		if err != nil {
			return nil, err
		}
		if spec.Kind == catalog.Image || spec.Kind == catalog.Video {
			_, _, err = modelrt.PrepareSD(ctx, spec)
			return spec, err
		}
		eng, err := modelrt.LlamaEngine()
		if err != nil {
			return nil, err
		}
		return spec, modelrt.Pull(ctx, spec, eng)
	}()
	if err != nil {
		events.Emit("error", map[string]any{"model": ref[0], "message": err.Error()})
		return err
	}
	events.Emit("installed", map[string]any{"model": spec.ID, "kind": spec.Kind.String()})
	switch spec.Kind {
	case catalog.Image:
		fmt.Fprintf(os.Stderr, "%s saved as %s — `fornax imagine %s \"…\"`\n", ui.Green("✓"), ui.Bold(spec.ID), spec.ID)
	case catalog.Video:
		fmt.Fprintf(os.Stderr, "%s saved as %s — `fornax animate %s \"…\"`\n", ui.Green("✓"), ui.Bold(spec.ID), spec.ID)
	default:
		fmt.Fprintf(os.Stderr, "%s saved as %s — `fornax ask %s …` / `fornax run %s`\n",
			ui.Green("✓"), ui.Bold(spec.ID), spec.ID, spec.ID)
	}
	return nil
}

type companionFlags []modelrt.Companion

func (c *companionFlags) String() string { return "" }

func (c *companionFlags) Set(value string) error {
	name, ref, ok := strings.Cut(value, "=")
	name = strings.TrimLeft(name, "-")
	if !ok || name == "" || ref == "" {
		return fmt.Errorf("--with wants <sd-cli flag>=hf:Org/Repo/File, got %q", value)
	}
	*c = append(*c, modelrt.Companion{Flag: name, Ref: ref})
	return nil
}

// `fornax pull ollama:<name>[:<tag>] [--as name] [--kind text|vision|audio]`
func cmdPullOllama(ctx context.Context, args []string) error {
	set := flag.NewFlagSet("pull", flag.ExitOnError)
	as := set.String("as", "", "custom id to save the model under")
	kind := set.String("kind", "", "text | vision | audio (default: text, or vision when the model ships a projector)")
	asEvents := set.Bool("events", false, "one JSON event per line on stdout")
	set.Usage = ui.UsageFunc(set, `usage: fornax pull ollama:<name>[:<tag>] [--as name] [--events]
       (or paste an ollama.com/library/<name> URL)`)
	ref := parseFlexible(set, args, 1)
	if len(ref) != 1 {
		return fmt.Errorf("usage: fornax pull ollama:<name>[:<tag>] [--as name] [--kind vision]")
	}
	if *asEvents {
		events.Enable()
	}
	spec, err := func() (*catalog.Spec, error) {
		spec, err := modelrt.EnsureOllama(ctx, ref[0], *as, *kind)
		if err != nil {
			return nil, err
		}
		eng, err := modelrt.LlamaEngine()
		if err != nil {
			return nil, err
		}
		return spec, modelrt.Pull(ctx, spec, eng)
	}()
	if err != nil {
		events.Emit("error", map[string]any{"model": ref[0], "message": err.Error()})
		return err
	}
	events.Emit("installed", map[string]any{"model": spec.ID, "kind": spec.Kind.String()})
	fmt.Fprintf(os.Stderr, "%s saved as %s — `fornax ask %s …` / `fornax run %s`\n",
		ui.Green("✓"), ui.Bold(spec.ID), spec.ID, spec.ID)
	return nil
}
