package modelrt

import (
	"regexp"
	"strings"
	"testing"
)

func TestSDEngineBuildsAreComplete(t *testing.T) {
	for platform, variants := range sdEngines {
		for _, eng := range variants {
			if (eng.Repo == "" || eng.Asset == "") == (eng.Image == "" || eng.ImageLayer == "") {
				t.Errorf("%s %s: an sd build comes from exactly one of a release asset or an image", platform, eng.DirName)
			}
			if eng.Binary == "" || eng.DirName == "" || eng.Backend == "" {
				t.Errorf("%s %s: incomplete sd build", platform, eng.DirName)
			}
			for _, part := range eng.Parts {
				if (part.Repo == "") == (part.PyPI == "") || part.Asset == "" {
					t.Errorf("%s: part %+v needs one source and a pattern", eng.DirName, part)
				}
			}
		}
	}
}

// sd's release names carry the commit and the runner's OS version.
func TestSDAssetsMatchUpstreamNames(t *testing.T) {
	want := map[string]string{
		"darwin/arm64/metal":   "sd-master-2f88688-bin-Darwin-macOS-26.6.2-arm64.zip",
		"linux/amd64/cpu":      "sd-master-2f88688-bin-Linux-Ubuntu-24.04-x86_64.zip",
		"linux/amd64/vulkan":   "sd-master-2f88688-bin-Linux-Ubuntu-24.04-x86_64-vulkan.zip",
		"windows/amd64/cpu":    "sd-master-2f88688-bin-win-cpu-x64.zip",
		"windows/amd64/cuda":   "sd-master-2f88688-bin-win-cuda12-x64.zip",
		"windows/amd64/vulkan": "sd-master-2f88688-bin-win-vulkan-x64.zip",
	}
	release := []string{"sd-master-2f88688-bin-Linux-Ubuntu-24.04-x86_64-rocm-7.14.0.zip", "sd-master-2f88688-bin-win-rocm-7.14.0-x64.zip"}
	for _, name := range want {
		release = append(release, name)
	}
	for platform, variants := range sdEngines {
		for _, eng := range variants {
			name, ok := want[platform+"/"+string(eng.Backend)]
			if !ok {
				continue
			}
			var got []string
			for _, candidate := range release {
				if regexp.MustCompile(eng.Asset).MatchString(candidate) {
					got = append(got, candidate)
				}
			}
			if len(got) != 1 || got[0] != name {
				t.Errorf("%s/%s matches %v, want %s", platform, eng.Backend, got, name)
			}
		}
	}
}

// A user-added model runs with the files and arguments saved beside it, and
// a flag given on the command line comes last so it wins.
func TestSDArgsCarryTheSavedRecipe(t *testing.T) {
	entry := customEntry{
		ID: "my-video", Kind: "video", Repo: "org/repo", File: "video.gguf",
		Companions: []customCompanion{{Flag: "vae", File: "vae.safetensors"}, {Flag: "t5xxl", File: "umt5.gguf"}},
		Args:       []string{"--steps", "4", "--cfg-scale", "1.0"},
	}
	spec := entry.spec()
	got := strings.Join(sdArgs("/root", spec, "a boat", "out.webm", 7, []string{"--steps", "8"}), " ")
	for _, want := range []string{
		"--diffusion-model /root/models/my-video/video.gguf",
		"-M vid_gen",
		"--vae /root/models/my-video/vae.safetensors",
		"--t5xxl /root/models/my-video/umt5.gguf",
		"--steps 4 --cfg-scale 1.0 --steps 8",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("sd-cli args %q are missing %q", got, want)
		}
	}
	checkpoint := (&customEntry{ID: "my-image", Kind: "image", File: "sd.safetensors"}).spec()
	if got := sdArgs("/root", checkpoint, "a door", "out.png", 7, nil); got[0] != "-m" {
		t.Fatalf("a lone checkpoint loads with -m, got %v", got[:2])
	}
}
