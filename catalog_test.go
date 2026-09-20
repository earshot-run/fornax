package main

import (
	"strings"
	"testing"
)

func TestEveryPinIsComplete(t *testing.T) {
	for _, spec := range models {
		if len(spec.sha256) != 64 {
			t.Errorf("%s: sha256 is not 64 hex chars", spec.id)
		}
		if !strings.Contains(spec.url, spec.revision) {
			t.Errorf("%s: url does not carry the pinned revision", spec.id)
		}
		if !strings.HasSuffix(spec.url, spec.file) {
			t.Errorf("%s: url does not end with the pinned file", spec.id)
		}
		if spec.bytes <= 0 || spec.port <= 1024 {
			t.Errorf("%s: incomplete pin", spec.id)
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
	if spec.binary == "" || spec.bytes <= 0 {
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
