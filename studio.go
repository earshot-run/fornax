package main

// `studio` takes its flags here; the studio itself is internal/studio.

import (
	"context"
	"flag"

	"github.com/earshot-run/fornax/internal/studio"
	"github.com/earshot-run/fornax/internal/ui"
)

func cmdStudio(ctx context.Context, args []string) error {
	set := flag.NewFlagSet("studio", flag.ExitOnError)
	port := set.Int("port", studio.Port, "loopback port (default 7340; with -on, the first tunnel port to try, from 7360)")
	noOpen := set.Bool("no-open", false, "print the link without opening a browser")
	on := set.String("on", "", "run the studio on another machine over ssh: [user@]host or an ssh-config alias")
	fornaxPath := set.String("fornax", "fornax", "fornax on the -on machine; ~/.local/bin and ~/go/bin are searched too")
	shown := flag.NewFlagSet("studio", flag.ContinueOnError)
	set.VisitAll(func(f *flag.Flag) { shown.Var(f.Value, f.Name, f.Usage) })
	set.Usage = ui.UsageFunc(shown, "usage: fornax studio [-port N] [-no-open] [-on [user@]host [-fornax path]]")
	// Hidden from help: the remote end of -on, which ends when stdin closes.
	leash := set.Bool("leash", false, "")
	set.Parse(args)
	if *on != "" {
		start := studio.RemotePortBase
		set.Visit(func(f *flag.Flag) {
			if f.Name == "port" {
				start = *port
			}
		})
		tag := ""
		if version != "dev" {
			tag = version
		}
		return studio.On(ctx, *on, *fornaxPath, start, *noOpen, tag)
	}
	return studio.Serve(ctx, *port, *noOpen, *leash)
}
