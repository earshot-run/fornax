package catalog

import (
	"regexp"
	"strings"
	"testing"
)

func TestEveryModelFileHasAURL(t *testing.T) {
	for _, spec := range models {
		for _, file := range spec.Files() {
			url := spec.URL(file)
			if !strings.HasPrefix(url, "https://") || !strings.HasSuffix(url, file.File) {
				t.Errorf("%s: url %s does not fetch %s", spec.ID, url, file.File)
			}
			if file.Bytes <= 0 {
				t.Errorf("%s: %s has no listed size", spec.ID, file.File)
			}
		}
		if spec.Runtime != SD && spec.Port <= 1024 {
			t.Errorf("%s: port %d is not usable", spec.ID, spec.Port)
		}
	}
	ids := map[string]bool{}
	ports := map[int]bool{}
	for _, spec := range models {
		if ids[spec.ID] {
			t.Errorf("duplicate model id %s", spec.ID)
		}
		ids[spec.ID] = true
		if ports[spec.Port] {
			t.Errorf("duplicate port %d", spec.Port)
		}
		ports[spec.Port] = true
	}
}

func TestModalityNeedsAProjector(t *testing.T) {
	for _, spec := range models {
		switch spec.Kind {
		case Vision, Audio, Speech:
			if spec.MMProj == nil {
				t.Errorf("%s: %s model has no projector pin", spec.ID, spec.Kind)
			}
		case Text, Decision, Image, Embed, Rerank:
			if spec.MMProj != nil {
				t.Errorf("%s: %s model carries a projector", spec.ID, spec.Kind)
			}
		default:
			t.Errorf("%s: unknown kind", spec.ID)
		}
	}
}

func TestKevTarballsComeFromGitHub(t *testing.T) {
	for _, spec := range models {
		if spec.Runtime != Kev {
			continue
		}
		if spec.DType != "" && spec.DType != "bf16" && spec.DType != "fp16" {
			t.Errorf("%s: unexpected dtype %q", spec.ID, spec.DType)
		}
		if spec.FitBytes <= spec.Model.Bytes {
			t.Errorf("%s: fitBytes should exceed the adapter-only tarball", spec.ID)
		}
		if !strings.HasPrefix(spec.Model.URL, "https://github.com/jaredpalmer/kev/releases/") {
			t.Errorf("%s: kev tarball does not come from kev's releases", spec.ID)
		}
	}
}

func TestEngineBuildsAreComplete(t *testing.T) {
	for platform, variants := range Engines() {
		if len(variants) == 0 || (variants[0].Backend != CPU && variants[0].Backend != Metal) {
			t.Errorf("%s: the plain build must come first", platform)
		}
		dirs := map[string]bool{}
		for _, spec := range variants {
			if spec.Repo == "" || spec.Binary == "" || spec.Bench == "" || spec.TTS == "" {
				t.Errorf("%s %s: incomplete engine build", platform, spec.Asset)
			}
			if _, err := regexp.Compile(spec.Asset); err != nil {
				t.Errorf("%s: bad asset pattern %s: %v", platform, spec.Asset, err)
			}
			if dirs[spec.DirName] {
				t.Errorf("%s: two builds share the directory %s", platform, spec.DirName)
			}
			dirs[spec.DirName] = true
			if (spec.Backend == CUDA) != (len(spec.Parts) > 0 && spec.MinDriver > 0) {
				t.Errorf("%s: a CUDA build needs its runtime part and a minimum driver", spec.Asset)
			}
			for _, part := range spec.Parts {
				if _, err := regexp.Compile(part.Asset); err != nil || part.Repo == "" {
					t.Errorf("%s: bad part %+v", spec.Asset, part)
				}
			}
		}
	}
}

// The patterns pick exactly the build they mean out of a real release.
func TestEngineAssetsMatchUpstreamNames(t *testing.T) {
	release := []string{
		"cudart-llama-b11200-bin-ubuntu-cuda-12.8-x64.tar.gz",
		"cudart-llama-b11200-bin-ubuntu-cuda-13.4-x64.tar.gz",
		"cudart-llama-bin-win-cuda-12.4-x64.zip",
		"llama-b11200-bin-linux-arm64-snapdragon.tar.gz",
		"llama-b11200-bin-macos-arm64.tar.gz",
		"llama-b11200-bin-ubuntu-cuda-12.8-x64.tar.gz",
		"llama-b11200-bin-ubuntu-cuda-13.4-x64.tar.gz",
		"llama-b11200-bin-ubuntu-vulkan-x64.tar.gz",
		"llama-b11200-bin-ubuntu-x64.tar.gz",
		"llama-b11200-bin-win-cuda-12.4-x64.zip",
		"llama-b11200-bin-win-cpu-x64.zip",
	}
	want := map[string]string{
		"linux/amd64/cpu":       "llama-b11200-bin-ubuntu-x64.tar.gz",
		"linux/amd64/cuda":      "llama-b11200-bin-ubuntu-cuda-12.8-x64.tar.gz",
		"linux/amd64/vulkan":    "llama-b11200-bin-ubuntu-vulkan-x64.tar.gz",
		"darwin/arm64/metal":    "llama-b11200-bin-macos-arm64.tar.gz",
		"windows/amd64/cpu":     "llama-b11200-bin-win-cpu-x64.zip",
		"windows/amd64/cuda":    "llama-b11200-bin-win-cuda-12.4-x64.zip",
		"linux/amd64/cuda/rt":   "cudart-llama-b11200-bin-ubuntu-cuda-12.8-x64.tar.gz",
		"windows/amd64/cuda/rt": "cudart-llama-bin-win-cuda-12.4-x64.zip",
	}
	matches := func(pattern string) []string {
		var found []string
		for _, name := range release {
			if regexp.MustCompile(pattern).MatchString(name) {
				found = append(found, name)
			}
		}
		return found
	}
	for platform, variants := range Engines() {
		for _, spec := range variants {
			key := platform + "/" + string(spec.Backend)
			if name, ok := want[key]; ok {
				if got := matches(spec.Asset); len(got) != 1 || got[0] != name {
					t.Errorf("%s matches %v, want %s", key, got, name)
				}
			}
			for _, part := range spec.Parts {
				if name, ok := want[key+"/rt"]; ok {
					if got := matches(part.Asset); len(got) != 1 || got[0] != name {
						t.Errorf("%s runtime matches %v, want %s", key, got, name)
					}
				}
			}
		}
	}
}

func TestFitUsesThisComputersRAM(t *testing.T) {
	cases := []struct {
		weight, memory int64
		want           Fit
	}{
		{14 * gib, 16 * gib, Tight},
		{19 * gib, 24 * gib, Tight},
		{19 * gib, 32 * gib, Fits},
		{52 * gib, 36 * gib, Wont},
		{65 * gib, 128 * gib, Fits},
		{14 * gib, 0, Unknown},
	}
	for _, c := range cases {
		if got := FitFor(c.weight, c.memory); got != c.want {
			t.Errorf("fit(%d, %d) = %v, want %v", c.weight, c.memory, got, c.want)
		}
	}
}

func TestNeededRAMRoundsUpToASizePeopleBuy(t *testing.T) {
	cases := [][2]int64{
		{14 * gib, 24 * gib},
		{19 * gib, 32 * gib},
		{52 * gib, 96 * gib},
		{65 * gib, 128 * gib},
	}
	for _, c := range cases {
		if got := NeededBytes(c[0]); got != c[1] {
			t.Errorf("NeededBytes(%d) = %d, want %d", c[0], got, c[1])
		}
	}
}
