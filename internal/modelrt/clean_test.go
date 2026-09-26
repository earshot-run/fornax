package modelrt

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/earshot-run/fornax/internal/paths"
)

// An engine dir no build installs to any more goes; a current one stays.
func TestCleanReapsRetiredEngineDirs(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FORNAX_HOME", root)
	for _, dir := range []string{"b11060", "sd-master-c678dfe-vulkan", "llama-cpu", "sd-cuda"} {
		if err := os.MkdirAll(filepath.Join(paths.EnginesDir(root), dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := Clean(false); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(paths.EnginesDir(root))
	var left []string
	for _, e := range entries {
		left = append(left, e.Name())
	}
	if len(left) != 2 || left[0] != "llama-cpu" || left[1] != "sd-cuda" {
		t.Fatalf("engine dirs left: %v", left)
	}
}
