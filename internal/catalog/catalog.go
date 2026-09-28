package catalog

import (
	"fmt"
	"regexp"
	"runtime"
	"strings"
)

// The built-ins: models that cannot be pulled from Hugging Face — kev
// checkpoints ship as GitHub release tarballs, laya is a safetensors tree
// behind its own runtime, apple-fm is in the OS. Every other model is a
// `pull hf:`/`pull ollama:` custom saved at fetch time in custom.json.

// ContextWindow is the default context every served model starts with.
const ContextWindow = 16_384

// What a model can take as input.
type Modality int

const (
	Text Modality = iota
	Vision
	Audio
	Decision
	Image
	Embed
	Speech
	Rerank
	Video
)

func (m Modality) String() string {
	switch m {
	case Vision:
		return "vision"
	case Audio:
		return "audio"
	case Decision:
		return "decision"
	case Image:
		return "image"
	case Embed:
		return "embed"
	case Speech:
		return "speech"
	case Rerank:
		return "rerank"
	case Video:
		return "video"
	}
	return "text"
}

// What serves the model. Llama is the llama.cpp binary;
// Kev is the python kev.serve app (see modelrt/kev.go); Apple is the
// on-device Foundation Models framework behind a compiled Swift bridge
// (see modelrt/apple.go). SD is a stable-diffusion.cpp binary — a
// foreground image and video generator, not a server (see modelrt/sd.go).
// Laya is the python laya package behind fornax's embedded serve shim
// (see modelrt/laya.go).
type Runtime int

const (
	Llama Runtime = iota
	Kev
	Apple
	SD
	Laya
)

func (r Runtime) String() string {
	return [...]string{"llama", "kev", "apple", "sd", "laya"}[r]
}

var quantPattern = regexp.MustCompile(`(?i)(I?Q\d+(_K)?_[A-Z0-9]+|MXFP4|F16|BF16)`)

// The weight format, read off the weights' file name.
func (spec *Spec) Quant() string {
	return strings.ToUpper(quantPattern.FindString(spec.Model.File))
}

// One file a model downloads.
type Artifact struct {
	// sd only: the engine flag a companion file is passed under.
	Flag string
	File string
	// A Hugging Face branch or commit; "" is main.
	Revision string
	// The size last seen upstream, for listings and the fit check; a
	// download learns the real size from the server.
	Bytes int64
	// Non-Hugging-Face sources set this directly (kev release tarballs).
	URL string
	// The sha256 the source publishes for these bytes (Hugging Face's LFS
	// etag, an OCI blob digest), checked after download. Empty when the
	// source publishes none.
	SHA256 string
}

type Spec struct {
	ID      string
	Name    string
	Summary string
	// Who trained it and how large it is, for front ends. Empty when unknown.
	Maker   string
	Params  string
	Kind    Modality
	Runtime Runtime
	Repo    string
	Model   Artifact
	// Companion projector (vision/audio). nil for text-only models.
	MMProj *Artifact
	// Further files the engine loads beside the weights, each passed under
	// the engine flag its Artifact names (a VAE, a text encoder).
	Companions []Artifact
	// Engine arguments saved with a user-added model.
	Args []string
	// kev only: KEV_DTYPE the server should run at ("" = fp32).
	DType string
	// kev only: the RAM the merged model actually wants — the tarball is
	// just the adapter, the base downloads separately and dwarfs it.
	FitBytes int64
	Port     int
}

func (spec *Spec) URL(file *Artifact) string {
	if file.URL != "" {
		return file.URL
	}
	revision := file.Revision
	if revision == "" {
		revision = "main"
	}
	return fmt.Sprintf("https://huggingface.co/%s/resolve/%s/%s", spec.Repo, revision, file.File)
}

// The disk/RAM footprint the fit check and `list` should warn about. For kev
// the downloaded adapter is small but the base model is not.
func (spec *Spec) SizeBytes() int64 {
	if spec.FitBytes > 0 {
		return spec.FitBytes
	}
	return spec.TotalBytes()
}

// Every artifact this model needs, main weights first. apple-fm ships in
// the OS — there is nothing to fetch.
func (spec *Spec) Files() []*Artifact {
	if spec.Runtime == Apple {
		return nil
	}
	files := []*Artifact{&spec.Model}
	if spec.MMProj != nil {
		files = append(files, spec.MMProj)
	}
	for i := range spec.Companions {
		files = append(files, &spec.Companions[i])
	}
	return files
}

func (spec *Spec) TotalBytes() int64 {
	total := spec.Model.Bytes
	if spec.MMProj != nil {
		total += spec.MMProj.Bytes
	}
	for _, companion := range spec.Companions {
		total += companion.Bytes
	}
	return total
}

// One engine build (llama.cpp or stable-diffusion.cpp) for one platform
// and one accelerator, found at install time in the newest upstream release
// that carries it.
type EngineSpec struct {
	// What it is, for messages: "llama.cpp", "stable-diffusion.cpp".
	Name string
	// The GitHub repo whose releases carry the build, and the pattern its
	// asset name matches.
	Repo  string
	Asset string
	// Instead of a release asset: a container image (ghcr.io/owner/repo:tag)
	// whose layer created by a step mentioning ImageLayer holds the build.
	Image      string
	ImageLayer string
	// The directory under engine/ it installs to; each backend gets its own
	// so switching accelerators never reuses the wrong build.
	DirName string
	// cpu, metal, vulkan or cuda.
	Backend Backend
	// cuda only: the oldest NVIDIA driver (major version) the build runs on.
	MinDriver int
	// Linux only: the oldest glibc the build loads against ("2.38"), set by
	// the Ubuntu release upstream builds it on.
	MinGlibc string
	// Archives unpacked beside Binary after the main one — the CUDA runtime
	// ships apart from the build.
	Parts []EnginePart
	// The binaries, found beside each other once the archive is unpacked.
	Binary string
	Bench  string
	TTS    string
}

// A further archive an engine build needs: a GitHub release asset of Repo,
// or the newest wheel of a PyPI package, whose file name matches Asset.
type EnginePart struct {
	Repo  string
	PyPI  string
	Asset string
	// Only this directory of the archive lands beside the binary; "" is all
	// of it, less any wrapping directories.
	Dir string
}

type Backend string

const (
	CPU    Backend = "cpu"
	Metal  Backend = "metal"
	Vulkan Backend = "vulkan"
	CUDA   Backend = "cuda"
)

var models = []Spec{
	{
		ID:      "kev-0.8b",
		Maker:   "Kev",
		Params:  "0.8B",
		Name:    "Kev 0.8B",
		Summary: "Typed questions, calibrated probabilities; the smallest kev.",
		Kind:    Decision,
		Runtime: Kev,
		Model: Artifact{
			File:  "kev-0.8b.tar.gz",
			Bytes: 45_807_488,
			URL:   "https://github.com/jaredpalmer/kev/releases/download/kev-family/kev-0.8b.tar.gz",
		},
		FitBytes: 4 * gib,
		Port:     7341,
	},
	{
		ID:      "kev-4b",
		Maker:   "Kev",
		Params:  "4B",
		Name:    "Kev 4B",
		Summary: "The kev to start with: best accuracy for its size.",
		Kind:    Decision,
		Runtime: Kev,
		Model: Artifact{
			File:  "kev-4b.tar.gz",
			Bytes: 128_973_435,
			URL:   "https://github.com/jaredpalmer/kev/releases/download/kev-family/kev-4b.tar.gz",
		},
		DType:    "bf16",
		FitBytes: 12 * gib,
		Port:     7342,
	},
	{
		ID:      "kev-9b",
		Maker:   "Kev",
		Params:  "9B",
		Name:    "Kev 9B",
		Summary: "Sharper answers than 4B; wants a 24 GB GPU.",
		Kind:    Decision,
		Runtime: Kev,
		Model: Artifact{
			File:  "kev-9b.tar.gz",
			Bytes: 172_224_408,
			URL:   "https://github.com/jaredpalmer/kev/releases/download/kev-family/kev-9b.tar.gz",
		},
		DType:    "bf16",
		FitBytes: 24 * gib,
		Port:     7343,
	},
	{
		ID:      "kev-27b",
		Maker:   "Kev",
		Params:  "27B",
		Name:    "Kev 27B",
		Summary: "The most accurate, best-calibrated kev; data-centre GPU.",
		Kind:    Decision,
		Runtime: Kev,
		Model: Artifact{
			File:  "kev-27b.tar.gz",
			Bytes: 446_278_982,
			URL:   "https://github.com/jaredpalmer/kev/releases/download/kev-family/kev-27b.tar.gz",
		},
		DType:    "bf16",
		FitBytes: 60 * gib,
		Port:     7344,
	},
	{
		ID:      "laya",
		Maker:   "Convai",
		Params:  "421M",
		Name:    "Laya",
		Summary: "Open-weights decision model on ModernBERT; the laya to start with.",
		Kind:    Decision,
		Runtime: Laya,
		Repo:    "convaiinnovations/laya",
		Model: Artifact{
			File:  "model.safetensors",
			Bytes: 842_609_210,
		},
		Companions: []Artifact{
			{File: "rl_agent_config.json", Bytes: 745},
			{File: "encoder/config.json", Bytes: 2083},
			{File: "tokenizer/tokenizer.json", Bytes: 3_583_228},
			{File: "tokenizer/tokenizer_config.json", Bytes: 308},
		},
		FitBytes: 3 * gib,
		Port:     7352,
	},
	{
		ID:      "laya-multilingual",
		Maker:   "Convai",
		Params:  "322M",
		Name:    "Laya Multilingual",
		Summary: "The laya decision model across 100+ languages on mmBERT.",
		Kind:    Decision,
		Runtime: Laya,
		Repo:    "convaiinnovations/laya",
		Model: Artifact{
			File:  "multilingual/model.safetensors",
			Bytes: 643_835_514,
		},
		Companions: []Artifact{
			{File: "multilingual/rl_agent_config.json", Bytes: 472},
			{File: "multilingual/encoder/config.json", Bytes: 1938},
			{File: "multilingual/tokenizer/tokenizer.json", Bytes: 34_363_188},
			{File: "multilingual/tokenizer/tokenizer_config.json", Bytes: 524},
		},
		FitBytes: 3 * gib,
		Port:     7353,
	},
	{
		ID:      "laya-typed-decisions",
		Maker:   "Convai",
		Params:  "421M",
		Name:    "Laya Typed-Decisions",
		Summary: "Laya fine-tuned on agent-trace, support, invoice and security workflows.",
		Kind:    Decision,
		Runtime: Laya,
		Repo:    "convaiinnovations/laya",
		Model: Artifact{
			File:  "typed-decisions/model.safetensors",
			Bytes: 842_609_220,
		},
		Companions: []Artifact{
			{File: "typed-decisions/rl_agent_config.json", Bytes: 847},
			{File: "typed-decisions/encoder/config.json", Bytes: 2084},
			{File: "typed-decisions/tokenizer/tokenizer.json", Bytes: 3_583_228},
			{File: "typed-decisions/tokenizer/tokenizer_config.json", Bytes: 337},
		},
		FitBytes: 3 * gib,
		Port:     7354,
	},
	{
		ID:      "apple-fm",
		Name:    "Apple Foundation Model",
		Summary: "The ~3B model inside macOS — nothing to download, always free.",
		Kind:    Text,
		Runtime: Apple,
		Port:    7351,
	},
}

// Model is a built-in entry, or nil. Models added with `pull hf:…` live in
// custom.json, not here — the caller merges those in.
func Model(id string) *Spec {
	for i := range models {
		if models[i].ID == id {
			return &models[i]
		}
	}
	return nil
}

// Models is the built-in set in listing order.
func Models() []*Spec {
	specs := make([]*Spec, len(models))
	for i := range models {
		specs[i] = &models[i]
	}
	return specs
}

// Every platform's llama.cpp builds, the plain one first. The CUDA runtime
// ships as its own asset beside each CUDA build.
var engines = func() map[string][]*EngineSpec {
	const repo = "ggml-org/llama.cpp"
	build := func(asset string, backend Backend) *EngineSpec {
		binary, bench, tts := "llama-server", "llama-bench", "llama-tts"
		if strings.HasSuffix(asset, ".zip") {
			binary, bench, tts = binary+".exe", bench+".exe", tts+".exe"
		}
		return &EngineSpec{
			Name:    "llama.cpp",
			Repo:    repo,
			Asset:   `^llama-[^-]+-bin-` + asset + `$`,
			DirName: "llama-" + string(backend),
			Backend: backend,
			Binary:  binary,
			Bench:   bench,
			TTS:     tts,
		}
	}
	cuda := func(eng *EngineSpec, minDriver int, runtime string) *EngineSpec {
		eng.MinDriver = minDriver
		eng.Parts = []EnginePart{{Repo: repo, Asset: `^cudart-llama-` + runtime + `$`}}
		return eng
	}
	glibc := func(eng *EngineSpec, version string) *EngineSpec {
		eng.MinGlibc = version
		return eng
	}
	return map[string][]*EngineSpec{
		"darwin/arm64": {build(`macos-arm64\.tar\.gz`, Metal)},
		"darwin/amd64": {build(`macos-x64\.tar\.gz`, CPU)},
		// Upstream builds the x64 CPU and Vulkan tarballs on Ubuntu 22.04 and
		// everything else on 24.04; a 24.04 build fails to load on 22.04 with
		// "GLIBC_2.38 not found".
		"linux/amd64": {
			glibc(build(`ubuntu-x64\.tar\.gz`, CPU), "2.35"),
			// CUDA 12 rather than 13: it runs on drivers from 570 up.
			glibc(cuda(build(`ubuntu-cuda-12\.\d+-x64\.tar\.gz`, CUDA), 570, `[^-]+-bin-ubuntu-cuda-12\.\d+-x64\.tar\.gz`), "2.38"),
			glibc(build(`ubuntu-vulkan-x64\.tar\.gz`, Vulkan), "2.35"),
		},
		"linux/arm64": {
			glibc(build(`ubuntu-arm64\.tar\.gz`, CPU), "2.38"),
			glibc(cuda(build(`ubuntu-cuda-13\.\d+-arm64\.tar\.gz`, CUDA), 580, `[^-]+-bin-ubuntu-cuda-13\.\d+-arm64\.tar\.gz`), "2.38"),
			glibc(build(`ubuntu-vulkan-arm64\.tar\.gz`, Vulkan), "2.38"),
		},
		"windows/amd64": {
			build(`win-cpu-x64\.zip`, CPU),
			cuda(build(`win-cuda-12\.\d+-x64\.zip`, CUDA), 551, `bin-win-cuda-12\.\d+-x64\.zip`),
			build(`win-vulkan-x64\.zip`, Vulkan),
		},
		"windows/arm64": {build(`win-cpu-arm64\.zip`, CPU)},
	}
}()

// This platform's llama.cpp builds, the plain one first; nil when there are none.
func EngineVariants() []*EngineSpec {
	return engines[runtime.GOOS+"/"+runtime.GOARCH]
}

// Engines is every platform's llama.cpp builds, keyed "goos/goarch".
func Engines() map[string][]*EngineSpec {
	return engines
}

const gib = 1 << 30

// The built-in a front end suggests first — free and already on the machine.
const StarterModel = "apple-fm"

// Leave room for the OS and the context cache; this is not a benchmark.
const (
	comfortable = 0.60
	tight       = 0.90
)

var ramSteps = []int64{
	8 * gib, 16 * gib, 24 * gib, 32 * gib, 36 * gib, 48 * gib,
	64 * gib, 96 * gib, 128 * gib, 192 * gib, 256 * gib, 512 * gib,
}

type Fit int

const (
	Fits Fit = iota
	Tight
	Wont
	Unknown
)

func (f Fit) String() string {
	switch f {
	case Fits:
		return "fits"
	case Tight:
		return "tight"
	case Wont:
		return "won't fit"
	}
	return "?"
}

func FitFor(weightBytes, memoryBytes int64) Fit {
	if memoryBytes == 0 || weightBytes == 0 {
		return Unknown
	}
	share := float64(weightBytes) / float64(memoryBytes)
	switch {
	case share <= comfortable:
		return Fits
	case share <= tight:
		return Tight
	}
	return Wont
}

func NeededBytes(weightBytes int64) int64 {
	min := int64(float64(weightBytes)/comfortable + 0.5)
	for _, step := range ramSteps {
		if step >= min {
			return step
		}
	}
	return min
}
