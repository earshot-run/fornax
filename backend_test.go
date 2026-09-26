package main

import (
	"strings"
	"testing"

	"github.com/earshot-run/fornax/internal/catalog"
)

func TestPickEngineFollowsTheHardware(t *testing.T) {
	cpu := &catalog.EngineSpec{Name: "llama.cpp", Backend: catalog.CPU}
	cuda := &catalog.EngineSpec{Name: "llama.cpp", Backend: catalog.CUDA, MinDriver: 570}
	vulkan := &catalog.EngineSpec{Name: "llama.cpp", Backend: catalog.Vulkan}
	all := []*catalog.EngineSpec{cpu, cuda, vulkan}
	cases := []struct {
		name  string
		probe gpuProbe
		want  *catalog.EngineSpec
	}{
		{"nothing", gpuProbe{}, cpu},
		{"nvidia", gpuProbe{nvidia: true, driver: 575, vulkan: true}, cuda},
		{"nvidia, unknown driver (WSL, Windows)", gpuProbe{nvidia: true}, cuda},
		{"nvidia too old for the CUDA build", gpuProbe{nvidia: true, driver: 535, vulkan: true}, vulkan},
		{"nvidia too old, no loader", gpuProbe{nvidia: true, driver: 535}, cpu},
		{"amd or intel with vulkan", gpuProbe{vulkan: true}, vulkan},
	}
	for _, c := range cases {
		if got, err := pickEngine(all, c.probe, ""); err != nil || got != c.want {
			t.Errorf("%s: got %v (%v), want %s", c.name, got.Backend, err, c.want.Backend)
		}
	}
	if got, _ := pickEngine([]*catalog.EngineSpec{cpu, vulkan}, gpuProbe{nvidia: true, vulkan: true}, ""); got != vulkan {
		t.Errorf("no CUDA build (sd on Linux): got %s, want vulkan", got.Backend)
	}
}

func TestPickEngineHonorsForcedBackend(t *testing.T) {
	metal := &catalog.EngineSpec{Name: "llama.cpp", Backend: catalog.Metal}
	cpu := &catalog.EngineSpec{Name: "llama.cpp", Backend: catalog.CPU}
	cuda := &catalog.EngineSpec{Name: "llama.cpp", Backend: catalog.CUDA}
	if got, err := pickEngine([]*catalog.EngineSpec{cpu, cuda}, gpuProbe{nvidia: true}, "CPU"); err != nil || got != cpu {
		t.Errorf("FORNAX_BACKEND=CPU: got %v, %v", got, err)
	}
	if got, err := pickEngine([]*catalog.EngineSpec{metal}, gpuProbe{}, "cpu"); err != nil || got != metal {
		t.Errorf("cpu on a Metal-only platform should take the plain build: %v, %v", got, err)
	}
	if _, err := pickEngine([]*catalog.EngineSpec{metal}, gpuProbe{}, "cuda"); err == nil || !strings.Contains(err.Error(), "have: metal") {
		t.Errorf("forcing a missing backend must fail clean, got %v", err)
	}
	if got, err := pickEngine(nil, gpuProbe{}, ""); got != nil || err != nil {
		t.Errorf("no builds: got %v, %v", got, err)
	}
}

func TestNVIDIADriverVersionParses(t *testing.T) {
	for line, want := range map[string]string{
		"NVRM version: NVIDIA UNIX x86_64 Kernel Module  570.86.15  Wed Jan 22 2026":                       "570",
		"NVRM version: NVIDIA UNIX Open Kernel Module for x86_64  575.51.03  Release Build  (dvs-builder)": "575",
	} {
		m := nvidiaVersionPattern.FindStringSubmatch(line)
		if m == nil || m[1] != want {
			t.Errorf("%q: got %v, want %s", line, m, want)
		}
	}
}

// On Ubuntu 22.04 (glibc 2.35) the 24.04-built CUDA build can't load, so an
// NVIDIA card falls back to the 22.04-built Vulkan one; with nothing
// loadable it fails clean before any download.
func TestPickEngineSkipsBuildsNewerThanGlibc(t *testing.T) {
	cpu := &catalog.EngineSpec{Name: "llama.cpp", Backend: catalog.CPU, MinGlibc: "2.35"}
	cuda := &catalog.EngineSpec{Name: "llama.cpp", Backend: catalog.CUDA, MinDriver: 570, MinGlibc: "2.38"}
	vulkan := &catalog.EngineSpec{Name: "llama.cpp", Backend: catalog.Vulkan, MinGlibc: "2.35"}
	jammy := gpuProbe{nvidia: true, vulkan: true, glibc: "2.35"}
	if got, err := pickEngine([]*catalog.EngineSpec{cpu, cuda, vulkan}, jammy, ""); err != nil || got != vulkan {
		t.Errorf("22.04 with NVIDIA: got %v, %v — want vulkan", got, err)
	}
	if got, _ := pickEngine([]*catalog.EngineSpec{cpu, cuda, vulkan}, gpuProbe{nvidia: true, vulkan: true, glibc: "2.39"}, ""); got != cuda {
		t.Errorf("24.04 with NVIDIA: got %v, want cuda", got)
	}
	if _, err := pickEngine([]*catalog.EngineSpec{cpu, cuda, vulkan}, jammy, "cuda"); err == nil {
		t.Error("forcing CUDA on glibc 2.35 must fail clean")
	}
	sd := &catalog.EngineSpec{Name: "stable-diffusion.cpp", Backend: catalog.CPU, MinGlibc: "2.38"}
	if _, err := pickEngine([]*catalog.EngineSpec{sd}, jammy, ""); err == nil || !strings.Contains(err.Error(), "glibc 2.38") || !strings.Contains(err.Error(), "has glibc 2.35") {
		t.Errorf("no loadable sd build: err = %v", err)
	}
	if got, _ := pickEngine([]*catalog.EngineSpec{sd}, gpuProbe{}, ""); got != sd {
		t.Error("an unknown glibc must not rule a build out")
	}
}

func TestVersionBefore(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{{"2.35", "2.38", true}, {"2.38", "2.35", false}, {"2.38", "2.38", false}, {"2.9", "2.10", true}, {"", "2.1", true}, {"2.38", "", false}} {
		if got := versionBefore(c.a, c.b); got != c.want {
			t.Errorf("versionBefore(%q, %q) = %v", c.a, c.b, got)
		}
	}
}

func TestSkippedGPUSaysWhyNotCUDA(t *testing.T) {
	cpu := &catalog.EngineSpec{Backend: catalog.CPU, MinGlibc: "2.35"}
	cuda := &catalog.EngineSpec{Backend: catalog.CUDA, MinDriver: 570, MinGlibc: "2.38"}
	variants := []*catalog.EngineSpec{cpu, cuda}
	if why := skippedGPU(variants, cpu, gpuProbe{nvidia: true, glibc: "2.35"}); !strings.Contains(why, "glibc 2.38") {
		t.Errorf("old glibc: %q", why)
	}
	if why := skippedGPU(variants, cpu, gpuProbe{nvidia: true, driver: 535, glibc: "2.39"}); !strings.Contains(why, "driver 570+") {
		t.Errorf("old driver: %q", why)
	}
	if why := skippedGPU(variants, cuda, gpuProbe{nvidia: true}); why != "" {
		t.Errorf("CUDA chosen: %q", why)
	}
	if why := skippedGPU(variants, cpu, gpuProbe{}); why != "" {
		t.Errorf("no NVIDIA: %q", why)
	}
}
