package catalog

import (
	"strings"
	"testing"
)

func TestEveryPinIsComplete(t *testing.T) {
	for _, spec := range models {
		for _, pin := range spec.Files() {
			url := spec.URL(pin)
			if len(pin.SHA256) != 64 {
				t.Errorf("%s: sha256 for %s is not 64 hex chars", spec.ID, pin.File)
			}
			if !strings.Contains(url, pin.Revision) {
				t.Errorf("%s: url for %s does not carry the pinned revision", spec.ID, pin.File)
			}
			if !strings.HasSuffix(url, pin.File) {
				t.Errorf("%s: url for %s does not end with the pinned file", spec.ID, pin.File)
			}
			if pin.Bytes <= 0 {
				t.Errorf("%s: incomplete pin for %s", spec.ID, pin.File)
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

func TestKevPinsPointAtGitHub(t *testing.T) {
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
			t.Errorf("%s: kev tarball does not come from the pinned release", spec.ID)
		}
	}
}

func TestEnginePinIsComplete(t *testing.T) {
	spec := Engine()
	if spec == nil {
		t.Skip("unsupported platform")
	}
	if len(spec.SHA256) != 64 {
		t.Error("sha256 is not 64 hex chars")
	}
	if !strings.HasSuffix(spec.URL, spec.Archive) {
		t.Error("url does not end with the pinned archive")
	}
	if spec.Binary == "" || spec.Bench == "" || spec.Bytes <= 0 {
		t.Error("incomplete engine pin")
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
