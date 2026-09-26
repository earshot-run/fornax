package main

// `fornax doctor` — what this machine can run, and what is set up already.

import (
	"fmt"
	"os/exec"
	"runtime"

	"github.com/earshot-run/fornax/internal/catalog"
	"github.com/earshot-run/fornax/internal/paths"
	"github.com/earshot-run/fornax/internal/ui"
)

func cmdDoctor(args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("usage: fornax doctor")
	}
	root := paths.Home()
	row := func(mark, label, value string) {
		fmt.Printf("  %s %s %s\n", mark, ui.Cell(label, 8, ui.Dim), value)
	}
	row(" ", "fornax", version)
	row(" ", "home", root)
	row(" ", "machine", fmt.Sprintf("%s %s · %s RAM", runtime.GOOS, runtime.GOARCH, ui.HumanSize(memoryBytes())))
	if eng := catalog.Engine(); eng != nil {
		if paths.EngineInstalled(root, eng) {
			row(ui.MarkOK(), "engine", "llama.cpp "+catalog.EngineVersion)
		} else {
			row(ui.MarkIdle(), "engine", "llama.cpp "+catalog.EngineVersion+ui.Dim(" — first pull downloads it"))
		}
	} else {
		row(ui.Red("✗"), "engine", "no pinned llama.cpp for this platform")
	}
	pyRuntime := func(name string, ready bool) {
		switch {
		case runtime.GOOS == "windows":
			row(ui.MarkIdle(), name, "needs macOS or Linux")
		default:
			if _, err := exec.LookPath("uv"); err != nil {
				row(ui.Red("✗"), name, "needs `uv` — https://docs.astral.sh/uv/")
			} else if ready {
				row(ui.MarkOK(), name, "runtime installed")
			} else {
				row(ui.MarkIdle(), name, ui.Dim(fmt.Sprintf("runtime not built yet — first %s command builds it", name)))
			}
		}
	}
	pyRuntime("kev", kevRuntimeReady(root))
	pyRuntime("laya", layaRuntimeReady(root))
	if appleSpec := model("apple-fm"); appleSpec != nil {
		switch err := appleSupported(); {
		case err != nil:
			row(ui.MarkIdle(), "apple-fm", ui.Dim("unsupported here — needs Apple Silicon on macOS 26+"))
		case appleInstalled(root, appleSpec):
			row(ui.MarkOK(), "apple-fm", "bridge compiled — ready to serve")
		default:
			row(ui.MarkIdle(), "apple-fm", ui.Dim("supported — `fornax pull apple-fm` compiles the bridge"))
		}
	}
	cfg, cfgErr := paths.LoadConfig(root)
	if cfgErr == nil && cfg.APIKey != "" {
		row(ui.MarkOK(), "key", "generated")
	} else {
		row(ui.MarkIdle(), "key", ui.Dim("generated on first run"))
	}
	for _, spec := range allSpecs(root) {
		if modelInstalled(root, spec) {
			live := ""
			if cfgErr == nil && isServing(spec, cfg.APIKey) {
				live = ui.Green(fmt.Sprintf(" — serving :%d", spec.Port))
			}
			row(ui.MarkOK(), "model", fmt.Sprintf("%s %s%s", spec.ID, ui.Dim("("+spec.Kind.String()+")"), live))
		}
	}
	if earshotPresent() {
		row(ui.MarkOK(), "earshot", "daemon config found")
	} else {
		row(ui.MarkIdle(), "earshot", ui.Dim("not found on this computer"))
	}
	return nil
}
