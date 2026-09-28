package main

// `run` and `connect` take their flags here; serving and handing a server to
// Earshot are modelrt.Serve and modelrt.Connect.

import (
	"context"
	"flag"
	"fmt"
	"strings"

	"github.com/earshot-run/fornax/internal/catalog"
	"github.com/earshot-run/fornax/internal/events"
	"github.com/earshot-run/fornax/internal/modelrt"
	"github.com/earshot-run/fornax/internal/ui"
)

const runUsage = "usage: fornax run <model> [-port N] [-ctx-size N] [-idle 20m] [-no-connect] [--events] [-- <llama-server flags>]"

func cmdRun(ctx context.Context, args []string) error {
	set := flag.NewFlagSet("run", flag.ExitOnError)
	port := set.Int("port", 0, "loopback port to serve on (default: the model's assigned port)")
	ctxSize := set.Int("ctx-size", catalog.ContextWindow, "context window passed to llama-server")
	noConnect := set.Bool("no-connect", false, "do not register the running server with Earshot")
	idle := set.Duration("idle", 0, "stop after this long without a request, e.g. 20m (default: never)")
	asEvents := set.Bool("events", false, "one JSON event per line on stdout; stops when the reader goes away")
	set.Usage = ui.UsageFunc(set, runUsage)
	head, extra := splitPassthrough(args)
	got := parseFlexible(set, head, 1)
	if len(got) != 1 {
		return fmt.Errorf("%s", runUsage)
	}
	id := got[0]
	if *asEvents {
		events.Enable()
	}
	ctxSet := false
	set.Visit(func(f *flag.Flag) {
		if f.Name == "ctx-size" {
			ctxSet = true
		}
	})
	err := modelrt.Serve(ctx, id, *port, *ctxSize, *noConnect, *idle, extra, ctxSet)
	if err != nil {
		events.Emit("error", map[string]any{"message": err.Error()})
	}
	return err
}

// Everything after a standalone `--` is passed through verbatim, so
// `fornax run m -- --flash-attn --cache-type-k q8_0` reaches llama-server.
func splitPassthrough(args []string) (head, extra []string) {
	for i, arg := range args {
		if arg == "--" {
			return args[:i], args[i+1:]
		}
	}
	return args, nil
}

func cmdConnect(args []string) error {
	set := flag.NewFlagSet("connect", flag.ExitOnError)
	port := set.Int("port", 0, "loopback port the model is served on (default: the model's assigned port)")
	set.Usage = ui.UsageFunc(set, "usage: fornax connect <model> [-port N]")
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
	return modelrt.Connect(id, *port)
}
