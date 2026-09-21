package main

import (
	"fmt"
	"runtime"
	"strings"
)

// The pinned catalog. Every artifact fornax will ever fetch is listed
// here with its immutable revision, exact byte count, and SHA-256 — nothing
// else is downloaded, and nothing downloaded is run before it matches all three.

const (
	engineVersion = "b11060"
	contextWindow = 16_384
)

// What a model can take as input.
type modality int

const (
	modalText modality = iota
	modalVision
	modalAudio
	modalDecision
	modalImage
	modalEmbed
)

func (m modality) String() string {
	switch m {
	case modalVision:
		return "vision"
	case modalAudio:
		return "audio"
	case modalDecision:
		return "decision"
	case modalImage:
		return "image"
	case modalEmbed:
		return "embed"
	}
	return "text"
}

// What serves the model. runtimeLlama is the pinned llama.cpp binary;
// runtimeKev is the python kev.serve app (see kev.go); runtimeApple is the
// on-device Foundation Models framework behind a compiled Swift bridge
// (see apple.go). runtimeSD is a pinned stable-diffusion.cpp binary — a
// foreground image generator, not a server (see draw.go).
type runtimeKind int

const (
	runtimeLlama runtimeKind = iota
	runtimeKev
	runtimeApple
	runtimeSD
)

type filePin struct {
	file     string
	revision string
	bytes    int64
	sha256   string
	// Non-Hugging-Face sources set this directly (kev release tarballs).
	url string
}

type modelSpec struct {
	id      string
	name    string
	summary string
	kind    modality
	rt      runtimeKind
	repo    string
	model   filePin
	// Companion projector (vision/audio). nil for text-only models.
	mmproj *filePin
	// kev only: KEV_DTYPE the server should run at ("" = fp32).
	dtype string
	// kev only: the RAM the merged model actually wants — the tarball is
	// just the adapter, the base downloads separately and dwarfs it.
	fitBytes int64
	port     int
}

func (spec *modelSpec) url(pin *filePin) string {
	if pin.url != "" {
		return pin.url
	}
	return fmt.Sprintf("https://huggingface.co/%s/resolve/%s/%s", spec.repo, pin.revision, pin.file)
}

// The disk/RAM footprint the fit check and `list` should warn about. For kev
// the downloaded adapter is small but the base model is not.
func (spec *modelSpec) sizeBytes() int64 {
	if spec.fitBytes > 0 {
		return spec.fitBytes
	}
	return spec.totalBytes()
}

// Every artifact this model needs, main weights first. apple-fm ships in
// the OS — there is nothing to fetch.
func (spec *modelSpec) files() []*filePin {
	if spec.rt == runtimeApple {
		return nil
	}
	if spec.mmproj != nil {
		return []*filePin{&spec.model, spec.mmproj}
	}
	return []*filePin{&spec.model}
}

func (spec *modelSpec) totalBytes() int64 {
	total := spec.model.bytes
	if spec.mmproj != nil {
		total += spec.mmproj.bytes
	}
	return total
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
	// The binaries inside the unpacked archive, relative to its root.
	binary string
	bench  string
}

var models = []modelSpec{
	{
		id:      "qwen3-1.7b",
		name:    "Qwen3 1.7B",
		summary: "Fastest replies; modest hardware.",
		kind:    modalText,
		repo:    "Qwen/Qwen3-1.7B-GGUF",
		model: filePin{
			file:     "Qwen3-1.7B-Q8_0.gguf",
			revision: "90862c4b9d2787eaed51d12237eafdfe7c5f6077",
			bytes:    1_834_426_016,
			sha256:   "061b54daade076b5d3362dac252678d17da8c68f07560be70818cace6590cb1a",
		},
		port: 7331,
	},
	{
		id:      "qwen3-4b",
		name:    "Qwen3 4B",
		summary: "A small local agent model for ordinary Macs.",
		kind:    modalText,
		repo:    "Qwen/Qwen3-4B-GGUF",
		model: filePin{
			file:     "Qwen3-4B-Q4_K_M.gguf",
			revision: "bc640142c66e1fdd12af0bd68f40445458f3869b",
			bytes:    2_497_280_256,
			sha256:   "7485fe6f11af29433bc51cab58009521f205840f5b4ae3a32fa7f92e8534fdf5",
		},
		port: 7332,
	},
	{
		id:      "qwen3-8b",
		name:    "Qwen3 8B",
		summary: "Sharper answers on 16 GB or more.",
		kind:    modalText,
		repo:    "Qwen/Qwen3-8B-GGUF",
		model: filePin{
			file:     "Qwen3-8B-Q4_K_M.gguf",
			revision: "7c41481f57cb95916b40956ab2f0b139b296d974",
			bytes:    5_027_783_488,
			sha256:   "d98cdcbd03e17ce47681435b5150e34c1417f50b5c0019dd560e4882c5745785",
		},
		port: 7333,
	},
	{
		id:      "qwen3-14b",
		name:    "Qwen3 14B",
		summary: "The sharpest local take; wants 24 GB or more.",
		kind:    modalText,
		repo:    "Qwen/Qwen3-14B-GGUF",
		model: filePin{
			file:     "Qwen3-14B-Q4_K_M.gguf",
			revision: "530227a7d994db8eca5ab5ced2fb692b614357fd",
			bytes:    9_001_752_960,
			sha256:   "500a8806e85ee9c83f3ae08420295592451379b4f8cf2d0f41c15dffeb6b81f0",
		},
		port: 7334,
	},
	{
		id:      "qwen3.5-0.8b",
		name:    "Qwen3.5 0.8B",
		summary: "Newest tiny Qwen; very fast.",
		kind:    modalText,
		repo:    "ggml-org/Qwen3.5-0.8B-GGUF",
		model: filePin{
			file:     "Qwen3.5-0.8B-Q8_0.gguf",
			revision: "8fea620810c4afa23dd6443f999a48574c1611a3",
			bytes:    833_592_096,
			sha256:   "37ae482d336108d23516fa35e8e0c4126688d81018b87178a18d752a1357814f",
		},
		port: 7337,
	},
	{
		id:      "ministral-3-8b",
		name:    "Ministral 3 8B",
		summary: "Mistral's current 8B instruct.",
		kind:    modalText,
		repo:    "ggml-org/Ministral-3-8B-Instruct-2512-GGUF",
		model: filePin{
			file:     "Ministral-3-8B-Instruct-2512-Q8_0.gguf",
			revision: "711997ebd75d80288650bf86a91c80ac73a7e529",
			bytes:    9_028_867_840,
			sha256:   "9f1f46ca2f659105e9fbbce630be190ffeaf4f4f746b2f9b716ddcf82e1802b4",
		},
		port: 7338,
	},
	{
		id:      "gpt-oss-20b",
		name:    "gpt-oss 20B",
		summary: "OpenAI's open MoE — 3.6B active, built for reasoning and tools.",
		kind:    modalText,
		repo:    "ggml-org/gpt-oss-20b-GGUF",
		model: filePin{
			file:     "gpt-oss-20b-MXFP4.gguf",
			revision: "ef9b12f2ff56c69cf32153a02784e7a3c88bf524",
			bytes:    12_109_566_624,
			sha256:   "27cd6c432c7672cb812a92f611cf3ba7bbc35928262bb1e1253ff4ee6ae35901",
		},
		port: 7339,
	},
	{
		id:      "gemma-4-e4b",
		name:    "Gemma 4 E4B",
		summary: "Google's multimodal 4B — text and images.",
		kind:    modalVision,
		repo:    "ggml-org/gemma-4-E4B-it-GGUF",
		model: filePin{
			file:     "gemma-4-E4B-it-Q8_0.gguf",
			revision: "b8093469224f83f5c38f691eb906c380e9e63114",
			bytes:    8_031_242_688,
			sha256:   "34be82b17b4942d389b9b527170c4b058027abdd32531fda063d3d97dd8ce80a",
		},
		mmproj: &filePin{
			file:     "mmproj-gemma-4-E4B-it-Q8_0.gguf",
			revision: "b8093469224f83f5c38f691eb906c380e9e63114",
			bytes:    559_874_816,
			sha256:   "197f49a93027f9843772bd24a6a9e0be2a32a788de5a3def330e9c585d86edd1",
		},
		port: 7340,
	},
	{
		id:      "qwen3.8-27b",
		name:    "Qwen3.8 27B",
		summary: "The current flagship local model; sees images too. Wants 32 GB.",
		kind:    modalVision,
		repo:    "ggml-org/Qwen3.8-27B-GGUF",
		model: filePin{
			file:     "Qwen3.8-27B-Q4_K_M.gguf",
			revision: "efbb3b1f70a21d97fd4495240648405f7228554f",
			bytes:    18_973_870_528,
			sha256:   "c600de0300ae8a0eb3a6c0b8b5561b8b96f16bd2c863c2a66c42de29d391a747",
		},
		mmproj: &filePin{
			file:     "mmproj-Qwen3.8-27B-Q8_0.gguf",
			revision: "efbb3b1f70a21d97fd4495240648405f7228554f",
			bytes:    629_247_008,
			sha256:   "2e968a6af97ce35d8971890b257b9b7edabf20ad91450501fa53162a19ee33eb",
		},
		port: 7344,
	},
	{
		id:      "qwen3-vl-2b",
		name:    "Qwen3-VL 2B",
		summary: "Current-gen vision-language at 2B.",
		kind:    modalVision,
		repo:    "ggml-org/Qwen3-VL-2B-Instruct-GGUF",
		model: filePin{
			file:     "Qwen3-VL-2B-Instruct-Q8_0.gguf",
			revision: "ea6a11058182570be6436b9a2e4ee7f7b49f908d",
			bytes:    1_834_427_296,
			sha256:   "b7802e29f71a9e5b5e3f83f613df898a2204342dcea71a231ea501d481813c39",
		},
		mmproj: &filePin{
			file:     "mmproj-Qwen3-VL-2B-Instruct-Q8_0.gguf",
			revision: "ea6a11058182570be6436b9a2e4ee7f7b49f908d",
			bytes:    445_053_056,
			sha256:   "69066c8f279ec85ff48ab4059f6ebba0d2932ca57667f2bbdac7d9805bca9e7b",
		},
		port: 7345,
	},
	{
		id:      "qwen2.5-vl-3b",
		name:    "Qwen2.5-VL 3B",
		summary: "Reads screenshots, photos and documents.",
		kind:    modalVision,
		repo:    "ggml-org/Qwen2.5-VL-3B-Instruct-GGUF",
		model: filePin{
			file:     "Qwen2.5-VL-3B-Instruct-Q4_K_M.gguf",
			revision: "5037fcf163dd95d1e41d1974465f0898ed108ca2",
			bytes:    1_929_901_056,
			sha256:   "d02fe9b69ad8cadbbd228e387667af66612c44bed29ffc8eb1e7caf9ac486c12",
		},
		mmproj: &filePin{
			file:     "mmproj-Qwen2.5-VL-3B-Instruct-Q8_0.gguf",
			revision: "5037fcf163dd95d1e41d1974465f0898ed108ca2",
			bytes:    844_757_728,
			sha256:   "980c9b2f78c04e6cff93d277ada09e768394f112d75db3b4e9dea8a69f9fb904",
		},
		port: 7335,
	},
	{
		id:      "ultravox-1b",
		name:    "Ultravox 1B",
		summary: "Hears audio takes; transcribes and answers about them.",
		kind:    modalAudio,
		repo:    "ggml-org/ultravox-v0_5-llama-3_2-1b-GGUF",
		model: filePin{
			file:     "Llama-3.2-1B-Instruct-Q4_K_M.gguf",
			revision: "5390c7c41cbd6f261f7f205fc0c5ae61bbdca650",
			bytes:    807_694_464,
			sha256:   "6f85a640a97cf2bf5b8e764087b1e83da0fdb51d7c9fab7d0fece9385611df83",
		},
		mmproj: &filePin{
			file:     "mmproj-ultravox-v0_5-llama-3_2-1b-f16.gguf",
			revision: "5390c7c41cbd6f261f7f205fc0c5ae61bbdca650",
			bytes:    1_371_123_616,
			sha256:   "b34dde1835752949d6b960528269af93c92fec91c61ea0534fcc73f96c1ed8b2",
		},
		port: 7336,
	},
	{
		id:      "qwen3-asr-0.6b",
		name:    "Qwen3-ASR 0.6B",
		summary: "Tiny dedicated speech-to-text.",
		kind:    modalAudio,
		repo:    "ggml-org/Qwen3-ASR-0.6B-GGUF",
		model: filePin{
			file:     "Qwen3-ASR-0.6B-Q8_0.gguf",
			revision: "928ab958557df9aa2ef1c93e0e83c7ad0933fae2",
			bytes:    804_749_248,
			sha256:   "bca259818b50ca7c4c05e9bdb35a5dc04fa039653a6d6f3f0f331f96f6aa1971",
		},
		mmproj: &filePin{
			file:     "mmproj-Qwen3-ASR-0.6B-Q8_0.gguf",
			revision: "928ab958557df9aa2ef1c93e0e83c7ad0933fae2",
			bytes:    214_392_480,
			sha256:   "41a342b5e4c514e968cb756de6cd1b7be39eff43c44c57a2ef5fc6522e36603d",
		},
		port: 7346,
	},
	{
		id:      "kev-0.6b",
		name:    "Kev 0.6B",
		summary: "Typed questions, calibrated probabilities; fastest kev.",
		kind:    modalDecision,
		rt:      runtimeKev,
		model: filePin{
			file:     "kev-0.6b.tar.gz",
			revision: "kev-family",
			bytes:    43_262_595,
			sha256:   "baf114336f20d5c21584d34cf516055b9d0cd7bc91634366a7fecb3f6cfd78fb",
			url:      "https://github.com/jaredpalmer/kev/releases/download/kev-family/kev-0.6b.tar.gz",
		},
		fitBytes: 3 * gib,
		port:     7341,
	},
	{
		id:      "kev-4b",
		name:    "Kev 4B",
		summary: "Best accuracy per byte of the kev family; the one to start with.",
		kind:    modalDecision,
		rt:      runtimeKev,
		model: filePin{
			file:     "kev-4b.tar.gz",
			revision: "kev-family",
			bytes:    131_245_471,
			sha256:   "01d3dc8eeccb4518e7053d94ce890d8ba7edbfa7d1c1687681c520dd1cde7769",
			url:      "https://github.com/jaredpalmer/kev/releases/download/kev-family/kev-4b.tar.gz",
		},
		dtype:    "bf16",
		fitBytes: 10 * gib,
		port:     7342,
	},
	{
		id:      "kev-8b",
		name:    "Kev 8B",
		summary: "Sharpest kev answers; wants a bigger machine.",
		kind:    modalDecision,
		rt:      runtimeKev,
		model: filePin{
			file:     "kev-8b.tar.gz",
			revision: "kev-family",
			bytes:    173_634_257,
			sha256:   "4dd002a09f61de311a6ecea8f8e9b343b5a647a2f4bb4f0f0d24bc8a79c0d4a6",
			url:      "https://github.com/jaredpalmer/kev/releases/download/kev-family/kev-8b.tar.gz",
		},
		dtype:    "bf16",
		fitBytes: 18 * gib,
		port:     7343,
	},
	{
		id:      "apple-fm",
		name:    "Apple Foundation Model",
		summary: "The ~3B model inside macOS — nothing to download, always free.",
		kind:    modalText,
		rt:      runtimeApple,
		port:    7351,
	},
}

// Catalog entries first, then anything saved in ~/.fornax/custom.json.
func model(id string) *modelSpec {
	for i := range models {
		if models[i].id == id {
			return &models[i]
		}
	}
	return customSpec(home(), id)
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
			bench:   "llama-b11060/llama-bench",
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
			bench:   "llama-bench.exe",
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
	var known []string
	var near string
	for _, spec := range allSpecs(home()) {
		known = append(known, spec.id)
		if near == "" && (strings.HasPrefix(spec.id, id) || strings.Contains(spec.id, id)) {
			near = spec.id
		}
	}
	if near != "" {
		return fmt.Errorf("unknown model %q — did you mean %q? (catalog: %s)", id, near, strings.Join(known, ", "))
	}
	return fmt.Errorf("unknown model %q (catalog: %s)", id, strings.Join(known, ", "))
}
