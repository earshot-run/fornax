package modelrt

// Resolving an argument to a model, and installing everything it needs.

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/earshot-run/fornax/internal/catalog"
	"github.com/earshot-run/fornax/internal/engine"
	"github.com/earshot-run/fornax/internal/paths"
	"github.com/earshot-run/fornax/internal/ui"
)

// An arg is a model reference when it carries an explicit scheme or is a
// bare Org/Repo — saved ids never contain a slash.
func IsHFRef(arg string) bool {
	return strings.HasPrefix(arg, "hf:") || strings.HasPrefix(arg, "hf.co/") ||
		strings.Contains(arg, "huggingface.co/") || strings.Contains(arg, "/")
}

func IsOllamaRef(arg string) bool {
	return strings.HasPrefix(arg, "ollama:") || strings.Contains(arg, "ollama.com/")
}

// Resolve maps an arg to a model: a saved id, a built-in, or a fresh
// hf:/ollama: ref saved on the spot.
func Resolve(ctx context.Context, arg string) (*catalog.Spec, *catalog.EngineSpec, error) {
	spec := Model(arg)
	if spec == nil {
		var entry *customEntry
		var err error
		switch {
		case IsOllamaRef(arg):
			entry, err = ensureOllama(ctx, arg, "", "")
		case IsHFRef(arg):
			entry, err = ensureHF(ctx, arg, "", "", "", "", nil, nil)
		default:
			return nil, nil, UnknownModel(arg)
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
		eng, err := SDEngine()
		if err != nil {
			return nil, nil, err
		}
		return spec, eng, nil
	}
	eng, err := LlamaEngine()
	if err != nil {
		return nil, nil, err
	}
	return spec, eng, nil
}

// This machine's llama.cpp build; an error means none fits.
func LlamaEngine() (*catalog.EngineSpec, error) {
	eng, err := pickEngine(catalog.EngineVariants(), ProbeGPU(), os.Getenv("FORNAX_BACKEND"))
	if err != nil {
		return nil, err
	}
	if eng == nil {
		return nil, fmt.Errorf("llama.cpp publishes no build for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	return eng, nil
}

// Install the engine, python runtime and weights spec needs; what is
// already installed is left alone.
func Pull(ctx context.Context, spec *catalog.Spec, eng *catalog.EngineSpec) error {
	root := paths.Home()
	if err := paths.ProtectDir(root); err != nil {
		return err
	}
	if spec.Runtime == catalog.Llama {
		if err := ensureEngine(ctx, root, eng, "engine"); err != nil {
			return err
		}
	}
	if spec.Runtime == catalog.Kev {
		if err := ensureKevRuntime(ctx, root); err != nil {
			return err
		}
	}
	if spec.Runtime == catalog.Laya {
		if err := ensureLayaRuntime(ctx, root); err != nil {
			return err
		}
	}
	if spec.Runtime == catalog.SD {
		if err := ensureEngine(ctx, root, eng, "sd engine"); err != nil {
			return err
		}
	}
	if Installed(root, spec) {
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
	if size := spec.TotalBytes(); size > 0 && Fit(size) == catalog.Wont {
		fmt.Fprintf(os.Stderr, "%s %s\n", ui.Yellow("warning:"),
			fmt.Sprintf("%s needs about %s and this machine has %s — it may not run", spec.ID, ui.HumanSize(size), ui.HumanSize(FitBudget())))
	}
	bar := ui.NewProgress(spec.ID, spec.TotalBytes())
	if err := ensureModel(ctx, root, spec, bar.Set); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "%s %s installed (%s)\n", ui.Green("✓"), ui.Bold(spec.ID), ui.Dim(ui.HumanSize(spec.TotalBytes())))
	return nil
}

// Install eng from upstream's newest release unless it is already here.
func ensureEngine(ctx context.Context, root string, eng *catalog.EngineSpec, label string) error {
	if paths.EngineInstalled(root, eng) {
		return nil
	}
	var bar *ui.Progress
	err := engine.Ensure(ctx, root, eng, func(done, total int64) {
		if bar == nil {
			bar = ui.NewProgress(label, total)
		}
		bar.Set(done)
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "%s engine %s %s (%s) installed\n", ui.Green("✓"), eng.Name, paths.EngineRelease(root, eng), eng.Backend)
	return nil
}
