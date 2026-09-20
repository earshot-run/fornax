package main

import (
	"fmt"
	"runtime"
	"strings"
)

// The pinned catalog. Every artifact earshot-local will ever fetch is listed
// here with its immutable revision, exact byte count, and SHA-256 — nothing
// else is downloaded, and nothing downloaded is run before it matches all three.

const (
	engineVersion = "b11060"
	contextWindow = 16_384
)

type modelSpec struct {
	id         string
	name       string
	summary    string
	file       string
	url        string
	revision   string
	repository string
	bytes      int64
	sha256     string
	port       int
}

type archiveKind int

const (
	archiveTarGz archiveKind = iota
	archiveZip
)

type engineSpec struct {
	url     string
	bytes   int64
	sha256  string
	archive string
	kind    archiveKind
	// The binary inside the unpacked archive, relative to its root.
	binary string
}

var models = []modelSpec{
	{
		id:         "qwen3-1.7b",
		name:       "Qwen3 1.7B",
		summary:    "Fastest replies; modest hardware.",
		file:       "Qwen3-1.7B-Q8_0.gguf",
		url:        "https://huggingface.co/Qwen/Qwen3-1.7B-GGUF/resolve/90862c4b9d2787eaed51d12237eafdfe7c5f6077/Qwen3-1.7B-Q8_0.gguf",
		revision:   "90862c4b9d2787eaed51d12237eafdfe7c5f6077",
		repository: "Qwen/Qwen3-1.7B-GGUF",
		bytes:      1_834_426_016,
		sha256:     "061b54daade076b5d3362dac252678d17da8c68f07560be70818cace6590cb1a",
		port:       7331,
	},
	{
		id:         "qwen3-4b",
		name:       "Qwen3 4B",
		summary:    "A small local agent model for ordinary Macs.",
		file:       "Qwen3-4B-Q4_K_M.gguf",
		url:        "https://huggingface.co/Qwen/Qwen3-4B-GGUF/resolve/bc640142c66e1fdd12af0bd68f40445458f3869b/Qwen3-4B-Q4_K_M.gguf",
		revision:   "bc640142c66e1fdd12af0bd68f40445458f3869b",
		repository: "Qwen/Qwen3-4B-GGUF",
		bytes:      2_497_280_256,
		sha256:     "7485fe6f11af29433bc51cab58009521f205840f5b4ae3a32fa7f92e8534fdf5",
		port:       7332,
	},
	{
		id:         "qwen3-8b",
		name:       "Qwen3 8B",
		summary:    "Sharper answers on 16 GB or more.",
		file:       "Qwen3-8B-Q4_K_M.gguf",
		url:        "https://huggingface.co/Qwen/Qwen3-8B-GGUF/resolve/7c41481f57cb95916b40956ab2f0b139b296d974/Qwen3-8B-Q4_K_M.gguf",
		revision:   "7c41481f57cb95916b40956ab2f0b139b296d974",
		repository: "Qwen/Qwen3-8B-GGUF",
		bytes:      5_027_783_488,
		sha256:     "d98cdcbd03e17ce47681435b5150e34c1417f50b5c0019dd560e4882c5745785",
		port:       7333,
	},
	{
		id:         "qwen3-14b",
		name:       "Qwen3 14B",
		summary:    "The sharpest local take; wants 24 GB or more.",
		file:       "Qwen3-14B-Q4_K_M.gguf",
		url:        "https://huggingface.co/Qwen/Qwen3-14B-GGUF/resolve/530227a7d994db8eca5ab5ced2fb692b614357fd/Qwen3-14B-Q4_K_M.gguf",
		revision:   "530227a7d994db8eca5ab5ced2fb692b614357fd",
		repository: "Qwen/Qwen3-14B-GGUF",
		bytes:      9_001_752_960,
		sha256:     "500a8806e85ee9c83f3ae08420295592451379b4f8cf2d0f41c15dffeb6b81f0",
		port:       7334,
	},
}

func model(id string) *modelSpec {
	for i := range models {
		if models[i].id == id {
			return &models[i]
		}
	}
	return nil
}

func engine() *engineSpec {
	const base = "https://github.com/ggml-org/llama.cpp/releases/download/b11060/"
	tarball := func(name string, bytes int64, sha256 string) *engineSpec {
		return &engineSpec{
			url:     base + "llama-b11060-bin-" + name + ".tar.gz",
			bytes:   bytes,
			sha256:  sha256,
			archive: "llama-b11060-bin-" + name + ".tar.gz",
			kind:    archiveTarGz,
			binary:  "llama-b11060/llama-server",
		}
	}
	zipball := func(name string, bytes int64, sha256 string) *engineSpec {
		return &engineSpec{
			url:     base + "llama-b11060-bin-" + name + ".zip",
			bytes:   bytes,
			sha256:  sha256,
			archive: "llama-b11060-bin-" + name + ".zip",
			kind:    archiveZip,
			binary:  "llama-server.exe",
		}
	}
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "darwin/arm64":
		return tarball("macos-arm64", 11_178_367, "f38d330eb9e097316cbda7838d99d5414ab4195f8f96b303949ec80547168592")
	case "darwin/amd64":
		return tarball("macos-x64", 11_216_404, "c3799ae5495069f6d54be6dd0835a487b28f66868a83a52f542aa65a7e711dc9")
	case "linux/amd64":
		return tarball("ubuntu-x64", 16_877_346, "0ef19058e60555e9a318baa9d8490335505e6207dafdb936fb07f9623368ec46")
	case "linux/arm64":
		return tarball("ubuntu-arm64", 13_498_203, "3c268f1a25f2c658eaf0c8b086bcfde9d1f40221802e87ae6dc2ae849b72daf1")
	case "windows/amd64":
		return zipball("win-cpu-x64", 18_463_147, "d0393b195149c4042349f8103cd6c7519546c71de4288a8a149de4c55366354b")
	case "windows/arm64":
		return zipball("win-cpu-arm64", 12_013_604, "dc07b5313dfdee239a7e06a58b766b1811da9102f9f46e59865c28e02d242e2a")
	}
	return nil
}

const gib = 1 << 30

// Leave room for the OS and the context cache; this is not a benchmark.
const (
	comfortable = 0.60
	tight       = 0.90
)

var ramSteps = []int64{
	8 * gib, 16 * gib, 24 * gib, 32 * gib, 36 * gib, 48 * gib,
	64 * gib, 96 * gib, 128 * gib, 192 * gib, 256 * gib, 512 * gib,
}

type fit int

const (
	fitFits fit = iota
	fitTight
	fitWont
	fitUnknown
)

func (f fit) String() string {
	switch f {
	case fitFits:
		return "fits"
	case fitTight:
		return "tight"
	case fitWont:
		return "won't fit"
	}
	return "?"
}

func modelFit(weightBytes, memoryBytes int64) fit {
	if memoryBytes == 0 || weightBytes == 0 {
		return fitUnknown
	}
	share := float64(weightBytes) / float64(memoryBytes)
	switch {
	case share <= comfortable:
		return fitFits
	case share <= tight:
		return fitTight
	}
	return fitWont
}

func neededBytes(weightBytes int64) int64 {
	min := int64(float64(weightBytes)/comfortable + 0.5)
	for _, step := range ramSteps {
		if step >= min {
			return step
		}
	}
	return min
}

func humanSize(bytes int64) string {
	if bytes >= gib {
		return fmt.Sprintf("%.1f GB", float64(bytes)/float64(gib))
	}
	return fmt.Sprintf("%.0f MB", float64(bytes)/float64(1<<20))
}

func unknownModel(id string) error {
	known := make([]string, 0, len(models))
	var near string
	for _, spec := range models {
		known = append(known, spec.id)
		if strings.HasPrefix(spec.id, id) || strings.Contains(spec.id, id) {
			near = spec.id
		}
	}
	if near != "" {
		return fmt.Errorf("unknown model %q — did you mean %q? (catalog: %s)", id, near, strings.Join(known, ", "))
	}
	return fmt.Errorf("unknown model %q (catalog: %s)", id, strings.Join(known, ", "))
}
