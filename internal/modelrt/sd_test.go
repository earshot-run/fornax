package modelrt

import (
	"strings"
	"testing"
)

func TestSDEnginePinsAreComplete(t *testing.T) {
	for platform, variants := range sdEngines {
		for _, eng := range variants {
			if len(eng.SHA256) != 64 || eng.Bytes <= 0 || eng.Binary == "" || eng.DirName == "" || eng.Backend == "" {
				t.Errorf("%s %s: incomplete sd engine pin", platform, eng.Archive)
			}
			if !strings.HasSuffix(eng.URL, eng.Archive) {
				t.Errorf("%s: url does not end with the pinned archive", eng.Archive)
			}
			for _, part := range eng.Parts {
				if len(part.SHA256) != 64 || part.Bytes <= 0 || !strings.HasSuffix(part.URL, part.Archive) {
					t.Errorf("%s: incomplete part %s", eng.Archive, part.Archive)
				}
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
		"--diffusion-model /root/models/my-video/video.gguf -M vid_gen",
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
