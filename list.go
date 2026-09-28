package main

// `fornax list` — every known model as a table, or one JSON object each.

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sync"

	"github.com/earshot-run/fornax/internal/catalog"
	"github.com/earshot-run/fornax/internal/modelrt"
	"github.com/earshot-run/fornax/internal/paths"
	"github.com/earshot-run/fornax/internal/ui"
)

func fitLabel(spec *catalog.Spec) (string, func(string) string) {
	if spec.Runtime == catalog.Apple {
		if modelrt.AppleSupported() == nil {
			return "fits", ui.Green
		}
		return "needs macOS 26+", ui.Red
	}
	switch modelrt.Fit(spec.SizeBytes()) {
	case catalog.Fits:
		return "fits", ui.Green
	case catalog.Tight:
		return "tight", ui.Yellow
	case catalog.Wont:
		return "needs " + ui.HumanSize(catalog.NeededBytes(spec.SizeBytes())) + "+", ui.Red
	}
	return "?", ui.Dim
}

func kindStyled(m catalog.Modality) func(string) string {
	switch m {
	case catalog.Vision:
		return ui.Magenta
	case catalog.Audio:
		return ui.Yellow
	case catalog.Decision:
		return ui.Green
	case catalog.Image, catalog.Video:
		return ui.Blue
	case catalog.Embed:
		return ui.Dim
	case catalog.Speech:
		return ui.Pink
	case catalog.Rerank:
		return ui.Dim
	}
	return ui.Cyan
}

func cmdList(args []string) error {
	set := flag.NewFlagSet("list", flag.ExitOnError)
	local := set.Bool("local", false, "installed models only")
	asJSON := set.Bool("json", false, "one JSON object per model on stdout")
	set.Usage = ui.UsageFunc(set, "usage: fornax list [--local] [--json]")
	set.Parse(args)
	if set.NArg() != 0 {
		return fmt.Errorf("usage: fornax list [--local] [--json]")
	}
	memory := modelrt.MemoryBytes()
	root := paths.Home()
	specs := modelrt.AllSpecs(root)
	if *local {
		var kept []*catalog.Spec
		for _, spec := range specs {
			if modelrt.Installed(root, spec) {
				kept = append(kept, spec)
			}
		}
		specs = kept
	}
	if *asJSON {
		cfg, err := paths.LoadConfig(root)
		if err != nil {
			return err
		}
		live := make([]bool, len(specs))
		var probes sync.WaitGroup
		for i, spec := range specs {
			probes.Add(1)
			go func(i int, spec *catalog.Spec) {
				defer probes.Done()
				live[i] = modelrt.IsServing(spec, cfg.APIKey)
			}(i, spec)
		}
		probes.Wait()
		enc := json.NewEncoder(os.Stdout)
		for i, spec := range specs {
			fit, _ := fitLabel(spec)
			enc.Encode(struct {
				ID        string `json:"id"`
				Name      string `json:"name"`
				Kind      string `json:"kind"`
				Runtime   string `json:"runtime"`
				Maker     string `json:"maker"`
				Params    string `json:"params"`
				Quant     string `json:"quant"`
				Needs     int64  `json:"needsBytes"`
				Machine   int64  `json:"machineBytes"`
				Starter   bool   `json:"starter"`
				Size      int64  `json:"bytes"`
				Partial   int64  `json:"partialBytes"`
				Fit       string `json:"fit"`
				Installed bool   `json:"installed"`
				Serving   bool   `json:"serving"`
				Port      int    `json:"port"`
				Repo      string `json:"repo"`
				Revision  string `json:"revision"`
				Summary   string `json:"summary"`
			}{spec.ID, spec.Name, spec.Kind.String(), spec.Runtime.String(), spec.Maker, spec.Params, spec.Quant(),
				catalog.NeededBytes(spec.SizeBytes()), memory, spec.ID == catalog.StarterModel, spec.SizeBytes(), paths.ModelPartialBytes(root, spec), fit,
				modelrt.Installed(root, spec), live[i], spec.Port,
				spec.Repo, spec.Model.Revision, spec.Summary})
		}
		return nil
	}
	// An id wider than the column would push every later cell out of line.
	idWidth := 15
	for _, spec := range specs {
		if width := len(spec.ID) + 1; width > idWidth {
			idWidth = width
		}
	}
	if len(specs) == 0 {
		fmt.Println(ui.Dim("  nothing to list — `fornax search <query>` finds models, `fornax pull hf:Org/Repo` gets one"))
	} else {
		fmt.Printf("  %s %s %s %s %s\n",
			ui.Cell("id", idWidth, ui.Dim), ui.Cell("kind", 8, ui.Dim), ui.Cell("size", 7, ui.Dim), ui.Cell("fit", 12, ui.Dim), ui.Dim("status"))
	}
	for _, spec := range specs {
		var status string
		switch partial := paths.ModelPartialBytes(root, spec); {
		case modelrt.Installed(root, spec):
			status = ui.MarkOK() + " installed"
		case partial > 0:
			status = ui.Yellow("◐") + fmt.Sprintf(" %d%%", partial*100/spec.TotalBytes())
		default:
			status = ui.MarkIdle()
		}
		fit, fitStyle := fitLabel(spec)
		size := ui.HumanSize(spec.SizeBytes())
		if spec.Runtime == catalog.Apple {
			size = "os"
		}
		fmt.Printf("  %s %s %s %s %s\n",
			ui.Cell(spec.ID, idWidth, ui.Bold),
			ui.Cell(spec.Kind.String(), 8, kindStyled(spec.Kind)),
			ui.Cell(size, 7, nil),
			ui.Cell(fit, 12, fitStyle),
			status)
		fmt.Printf("  %s %s\n", ui.Cell("", idWidth, nil), ui.Dim(spec.Summary))
	}
	engineState := "unsupported platform"
	if eng, err := modelrt.LlamaEngine(); err == nil {
		engineState = string(eng.Backend)
		if release := paths.EngineRelease(root, eng); release != "" {
			engineState = release + ", " + engineState
		}
	}
	fmt.Printf("\n  %s\n", ui.Dim(fmt.Sprintf("this machine: %s RAM · llama.cpp (%s)",
		ui.HumanSize(memory), engineState)))
	return nil
}
