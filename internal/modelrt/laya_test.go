package modelrt

import (
	"testing"

	"github.com/earshot-run/fornax/internal/catalog"
)

func TestLayaSourcePinIsComplete(t *testing.T) {
	if len(layaSource.SHA256) != 64 || layaSource.Bytes <= 0 {
		t.Error("incomplete laya source pin")
	}
	if layaSource.URL == "" && layaSource.Revision == "" {
		t.Error("the laya source pin names neither a URL nor a revision")
	}
}

// A laya model installs a checkpoint dir the shim can --model: weights,
// rl_agent_config.json, encoder config and the tokenizer pair — each pinned.
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
		for _, pin := range spec.Files() {
			files[pin.File] = true
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
				t.Errorf("%s: no pinned file ending in %s", spec.ID, wantFile)
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
