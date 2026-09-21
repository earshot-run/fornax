package main

import (
	"strings"
	"testing"
)

func TestEveryPinIsComplete(t *testing.T) {
	for _, spec := range models {
		for _, pin := range spec.files() {
			url := spec.url(pin)
			if len(pin.sha256) != 64 {
				t.Errorf("%s: sha256 for %s is not 64 hex chars", spec.id, pin.file)
			}
			if !strings.Contains(url, pin.revision) {
				t.Errorf("%s: url for %s does not carry the pinned revision", spec.id, pin.file)
			}
			if !strings.HasSuffix(url, pin.file) {
				t.Errorf("%s: url for %s does not end with the pinned file", spec.id, pin.file)
			}
			if pin.bytes <= 0 {
				t.Errorf("%s: incomplete pin for %s", spec.id, pin.file)
			}
		}
		if spec.rt != runtimeSD && spec.port <= 1024 {
			t.Errorf("%s: port %d is not usable", spec.id, spec.port)
		}
	}
	ids := map[string]bool{}
	ports := map[int]bool{}
	for _, spec := range models {
		if ids[spec.id] {
			t.Errorf("duplicate model id %s", spec.id)
		}
		ids[spec.id] = true
		if ports[spec.port] {
			t.Errorf("duplicate port %d", spec.port)
		}
		ports[spec.port] = true
	}
}

func TestModalityNeedsAProjector(t *testing.T) {
	for _, spec := range models {
		switch spec.kind {
		case modalVision, modalAudio, modalSpeech:
			if spec.mmproj == nil {
				t.Errorf("%s: %s model has no projector pin", spec.id, spec.kind)
			}
		case modalText, modalDecision, modalImage, modalEmbed, modalRerank:
			if spec.mmproj != nil {
				t.Errorf("%s: %s model carries a projector", spec.id, spec.kind)
			}
		default:
			t.Errorf("%s: unknown kind", spec.id)
		}
	}
}

func TestKevPinsPointAtGitHub(t *testing.T) {
	for _, spec := range models {
		if spec.rt != runtimeKev {
			continue
		}
		if spec.dtype != "" && spec.dtype != "bf16" && spec.dtype != "fp16" {
			t.Errorf("%s: unexpected dtype %q", spec.id, spec.dtype)
		}
		if spec.fitBytes <= spec.model.bytes {
			t.Errorf("%s: fitBytes should exceed the adapter-only tarball", spec.id)
		}
		if !strings.HasPrefix(spec.model.url, "https://github.com/jaredpalmer/kev/releases/") {
			t.Errorf("%s: kev tarball does not come from the pinned release", spec.id)
		}
	}
	if len(kevSource.sha256) != 64 || kevSource.bytes <= 0 {
		t.Error("incomplete kev source pin")
	}
}

func TestEnginePinIsComplete(t *testing.T) {
	spec := engine()
	if spec == nil {
		t.Skip("unsupported platform")
	}
	if len(spec.sha256) != 64 {
		t.Error("sha256 is not 64 hex chars")
	}
	if !strings.HasSuffix(spec.url, spec.archive) {
		t.Error("url does not end with the pinned archive")
	}
	if spec.binary == "" || spec.bench == "" || spec.bytes <= 0 {
		t.Error("incomplete engine pin")
	}
}

func TestFitUsesThisComputersRAM(t *testing.T) {
	cases := []struct {
		weight, memory int64
		want           fit
	}{
		{14 * gib, 16 * gib, fitTight},
		{19 * gib, 24 * gib, fitTight},
		{19 * gib, 32 * gib, fitFits},
		{52 * gib, 36 * gib, fitWont},
		{65 * gib, 128 * gib, fitFits},
		{14 * gib, 0, fitUnknown},
	}
	for _, c := range cases {
		if got := modelFit(c.weight, c.memory); got != c.want {
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
		if got := neededBytes(c[0]); got != c[1] {
			t.Errorf("neededBytes(%d) = %d, want %d", c[0], got, c[1])
		}
	}
}
