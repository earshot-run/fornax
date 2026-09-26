package modelrt

import (
	"testing"

	"github.com/earshot-run/fornax/internal/catalog"
)

// A laya model installs a checkpoint dir the shim can --model: weights,
// rl_agent_config.json, encoder config and the tokenizer pair.
func TestLayaSpecsCarryACheckpoint(t *testing.T) {
	want := []string{
		"model.safetensors",
		"rl_agent_config.json",
		"encoder/config.json",
		"tokenizer/tokenizer.json",
		"tokenizer/tokenizer_config.json",
	}
	for _, spec := range catalog.Models() {
		if spec.Runtime != catalog.Laya {
			continue
		}
		if spec.Kind != catalog.Decision {
			t.Errorf("%s: laya runtime but kind %s", spec.ID, spec.Kind)
		}
		files := map[string]bool{}
		for _, file := range spec.Files() {
			files[file.File] = true
		}
		prefix := ""
		for _, wantFile := range want {
			found := files[wantFile]
			if !found {
				// The bundled checkpoints nest every file one level down.
				for f := range files {
					if len(f) > len(wantFile) && f[len(f)-len(wantFile):] == wantFile {
						prefix = f[:len(f)-len(wantFile)]
						found = true
						break
					}
				}
			}
			if !found {
				t.Errorf("%s: no file ending in %s", spec.ID, wantFile)
			}
		}
		if prefix != "" {
			for f := range files {
				if f[:len(prefix)] != prefix {
					t.Errorf("%s: %s sits outside the %s checkpoint dir", spec.ID, f, prefix)
				}
			}
		}
	}
}
