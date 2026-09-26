package main

// Which engine build drives this machine's accelerator. Each platform pins a
// plain build plus GPU ones (catalog.Engines, sdEngines); this picks among
// them from what the machine has, or from FORNAX_BACKEND.

import (
	"debug/elf"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"

	"github.com/earshot-run/fornax/internal/catalog"
)

// What the machine offers an engine, found by looking for driver files
// rather than running anything.
type gpuProbe struct {
	nvidia bool
	// NVIDIA driver major version; 0 when unknown (Windows, WSL).
	driver int
	vulkan bool
	// Linux: the newest glibc version libc.so.6 defines ("2.35"); empty when
	// unknown (not Linux, musl, unreadable) — then no build is ruled out.
	glibc string
}

var nvidiaVersionPattern = regexp.MustCompile(`Kernel Module(?:\s+for\s+\S+)?\s+(\d+)\.`)

func probeGPU() gpuProbe {
	var probe gpuProbe
	switch runtime.GOOS {
	case "linux":
		probe.glibc = glibcVersion()
		if raw, err := os.ReadFile("/proc/driver/nvidia/version"); err == nil {
			probe.nvidia = true
			if m := nvidiaVersionPattern.FindSubmatch(raw); m != nil {
				probe.driver, _ = strconv.Atoi(string(m[1]))
			}
		}
		// WSL2 has no /proc/driver/nvidia; the Windows driver maps libcuda in here.
		if exists("/usr/lib/wsl/lib/libcuda.so.1") {
			probe.nvidia = true
		}
		// The loader alone proves nothing: Mesa ships lavapipe, a CPU Vulkan
		// driver, so it also needs a GPU behind it — a DRM render node, or
		// WSL's /dev/dxg with Mesa's D3D12-backed dzn driver. WSL has no
		// NVIDIA Vulkan driver at all.
		loader := false
		for _, dir := range []string{"/usr/lib/x86_64-linux-gnu", "/usr/lib/aarch64-linux-gnu", "/usr/lib64", "/usr/lib", "/usr/local/lib"} {
			if exists(filepath.Join(dir, "libvulkan.so.1")) {
				loader = true
				break
			}
		}
		render, _ := filepath.Glob("/dev/dri/renderD*")
		dzn, _ := filepath.Glob("/usr/share/vulkan/icd.d/dzn_icd*.json")
		probe.vulkan = loader && (len(render) > 0 || (exists("/dev/dxg") && len(dzn) > 0))
	case "windows":
		system := filepath.Join(os.Getenv("SystemRoot"), "System32")
		probe.nvidia = exists(filepath.Join(system, "nvcuda.dll"))
		probe.vulkan = exists(filepath.Join(system, "vulkan-1.dll"))
	}
	return probe
}

// Read off libc.so.6's own version definitions (GLIBC_2.35, …), so nothing
// has to run to learn it.
func glibcVersion() string {
	for _, path := range []string{"/lib/x86_64-linux-gnu/libc.so.6", "/lib/aarch64-linux-gnu/libc.so.6", "/lib64/libc.so.6", "/usr/lib64/libc.so.6", "/lib/libc.so.6", "/usr/lib/libc.so.6"} {
		file, err := elf.Open(path)
		if err != nil {
			continue
		}
		versions, err := file.DynamicVersions()
		file.Close()
		if err != nil {
			continue
		}
		newest := ""
		for _, v := range versions {
			if version, ok := strings.CutPrefix(v.Name, "GLIBC_"); ok && versionBefore(newest, version) {
				newest = version
			}
		}
		if newest != "" {
			return newest
		}
	}
	return ""
}

// Dotted numeric versions: "2.35" before "2.38"; "" before anything.
func versionBefore(a, b string) bool {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	if a == "" {
		return b != ""
	}
	for i := 0; i < len(as) || i < len(bs); i++ {
		var x, y int
		if i < len(as) {
			x, _ = strconv.Atoi(as[i])
		}
		if i < len(bs) {
			y, _ = strconv.Atoi(bs[i])
		}
		if x != y {
			return x < y
		}
	}
	return false
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// The build to run from variants (plain first). FORNAX_BACKEND=cpu|cuda|vulkan
// forces one and fails clean when this platform has no such build.
func pickEngine(variants []*catalog.EngineSpec, probe gpuProbe, forced string) (*catalog.EngineSpec, error) {
	if len(variants) == 0 {
		return nil, nil
	}
	// A build linked against a newer glibc than this Linux has cannot load.
	var loadable []*catalog.EngineSpec
	for _, eng := range variants {
		if probe.glibc == "" || eng.MinGlibc == "" || !versionBefore(probe.glibc, eng.MinGlibc) {
			loadable = append(loadable, eng)
		}
	}
	if len(loadable) == 0 {
		return nil, fmt.Errorf("%s's Linux builds need glibc %s or newer (Ubuntu 24.04, Debian 13, Fedora 39+); this machine has glibc %s",
			variants[0].Name, variants[0].MinGlibc, probe.glibc)
	}
	if forced = strings.ToLower(strings.TrimSpace(forced)); forced != "" {
		for _, eng := range loadable {
			if string(eng.Backend) == forced || (forced == "cpu" && eng.Backend == catalog.Metal) {
				return eng, nil
			}
		}
		var have []string
		for _, eng := range loadable {
			have = append(have, string(eng.Backend))
		}
		return nil, fmt.Errorf("FORNAX_BACKEND=%s: no %s build of %s for %s/%s (have: %s)",
			forced, forced, variants[0].Name, runtime.GOOS, runtime.GOARCH, strings.Join(have, ", "))
	}
	find := func(backend catalog.Backend) *catalog.EngineSpec {
		for _, eng := range loadable {
			if eng.Backend == backend {
				return eng
			}
		}
		return nil
	}
	if eng := find(catalog.CUDA); eng != nil && probe.nvidia && (probe.driver == 0 || probe.driver >= eng.MinDriver) {
		return eng, nil
	}
	// NVIDIA drivers ship a Vulkan ICD too, so an old driver or a missing
	// CUDA build still gets the GPU through Vulkan when the loader is there.
	if eng := find(catalog.Vulkan); eng != nil && probe.vulkan {
		return eng, nil
	}
	return loadable[0], nil
}

// The environment an engine binary runs with: the scrubbed one, plus the
// binary's own directory on the Linux library path, where the CUDA runtime
// that ships beside a GPU build lives.
func engineEnv(root, binary string) []string {
	env := scrubbedEnv(root)
	if runtime.GOOS == "linux" {
		env = append(env, "LD_LIBRARY_PATH="+filepath.Dir(binary))
	}
	return env
}

// For doctor: what the probe found, in words.
func describeGPU(probe gpuProbe) string {
	var found []string
	if probe.nvidia {
		nvidia := "NVIDIA driver"
		if probe.driver > 0 {
			nvidia += fmt.Sprintf(" %d", probe.driver)
		}
		found = append(found, nvidia)
	}
	if probe.vulkan {
		found = append(found, "Vulkan GPU")
	}
	if runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" {
		found = append(found, "Metal")
	}
	if probe.glibc != "" {
		found = append(found, "glibc "+probe.glibc)
	}
	if len(found) == 0 {
		return "none found — engines run on the CPU (FORNAX_BACKEND overrides)"
	}
	return strings.Join(found, ", ")
}

// Why the accelerator the machine has isn't the one running, or "".
func skippedGPU(variants []*catalog.EngineSpec, chosen *catalog.EngineSpec, probe gpuProbe) string {
	if chosen == nil || chosen.Backend == catalog.CUDA || !probe.nvidia {
		return ""
	}
	for _, eng := range variants {
		if eng.Backend != catalog.CUDA {
			continue
		}
		if probe.glibc != "" && eng.MinGlibc != "" && versionBefore(probe.glibc, eng.MinGlibc) {
			return fmt.Sprintf("the CUDA build needs glibc %s, this machine has %s (Ubuntu 24.04+ runs it)", eng.MinGlibc, probe.glibc)
		}
		if probe.driver > 0 && probe.driver < eng.MinDriver {
			return fmt.Sprintf("the CUDA build needs NVIDIA driver %d+, this machine has %d", eng.MinDriver, probe.driver)
		}
		return ""
	}
	return ""
}
