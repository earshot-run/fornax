package modelrt

import (
	"context"
	"fmt"
	"os"

	"github.com/earshot-run/fornax/internal/catalog"
	"github.com/earshot-run/fornax/internal/engine"
	"github.com/earshot-run/fornax/internal/paths"
	"github.com/earshot-run/fornax/internal/ui"
)

// RefreshEngines reinstalls this machine's engine builds from the newest
// upstream release that carries them, replacing whatever is installed —
// engines are used until removed, so this is the only way to move one
// forward. One line per engine, naming the release it moved from and to.
func RefreshEngines(ctx context.Context) ([]string, error) {
	root := paths.Home()
	type target struct {
		label string
		eng   *catalog.EngineSpec
	}
	var targets []target
	if eng, err := LlamaEngine(); err == nil && eng != nil {
		targets = append(targets, target{"llama.cpp", eng})
	}
	if eng, err := SDEngine(); err == nil && eng != nil {
		targets = append(targets, target{"stable-diffusion.cpp", eng})
	}
	if len(targets) == 0 {
		return nil, fmt.Errorf("this machine has no engine build to refresh")
	}
	var lines []string
	for _, t := range targets {
		before := paths.EngineRelease(root, t.eng)
		if err := os.RemoveAll(paths.EngineDir(root, t.eng)); err != nil {
			return lines, fmt.Errorf("could not remove the installed %s: %w", t.label, err)
		}
		var bar *ui.Progress
		if err := engine.Ensure(ctx, root, t.eng, func(done, total int64) {
			if bar == nil {
				bar = ui.NewProgress(t.label, total)
			}
			bar.Set(done)
		}); err != nil {
			return lines, err
		}
		after := paths.EngineRelease(root, t.eng)
		lines = append(lines, fmt.Sprintf("%s %s → %s (%s)", t.label, orNone(before), orNone(after), t.eng.Backend))
	}
	return lines, nil
}

func orNone(release string) string {
	if release == "" {
		return "none"
	}
	return release
}
