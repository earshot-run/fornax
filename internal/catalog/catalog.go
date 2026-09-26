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
// `pull hf:`/`pull ollama:` custom pinned at fetch time in custom.json.
// Every artifact still carries an immutable revision, exact byte count, and
// SHA-256 — nothing downloaded is run before it matches all three.

const (
	// EngineVersion is the one llama.cpp release fornax runs.
	EngineVersion = "b11060"
	// ContextWindow is the default context every served model starts with.
	ContextWindow = 16_384
)

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

// What serves the model. Llama is the pinned llama.cpp binary;
// Kev is the python kev.serve app (see kev.go); Apple is the
// on-device Foundation Models framework behind a compiled Swift bridge
// (see apple.go). SD is a pinned stable-diffusion.cpp binary — a
// foreground image and video generator, not a server (see imagine.go).
// Laya is the python laya package behind fornax's embedded serve shim
// (see laya.go).
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

// The weight format, read off the pinned file name.
func (spec *Spec) Quant() string {
	return strings.ToUpper(quantPattern.FindString(spec.Model.File))
}

type Pin struct {
	// sd only: the engine flag a companion file is passed under.
	Flag     string
	File     string
	Revision string
	Bytes    int64
	SHA256   string
	// Non-Hugging-Face sources set this directly (kev release tarballs).
	URL string
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
	Model   Pin
	// Companion projector (vision/audio). nil for text-only models.
	MMProj *Pin
	// Further files the engine loads beside the weights, each passed under
	// the engine flag its Pin names (a VAE, a text encoder).
	Companions []Pin
	// Engine arguments saved with a user-added model.
	Args []string
	// kev only: KEV_DTYPE the server should run at ("" = fp32).
	DType string
	// kev only: the RAM the merged model actually wants — the tarball is
	// just the adapter, the base downloads separately and dwarfs it.
	FitBytes int64
	Port     int
}

func (spec *Spec) URL(pin *Pin) string {
	if pin.URL != "" {
		return pin.URL
	}
	return fmt.Sprintf("https://huggingface.co/%s/resolve/%s/%s", spec.Repo, pin.Revision, pin.File)
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
func (spec *Spec) Files() []*Pin {
	if spec.Runtime == Apple {
		return nil
	}
	files := []*Pin{&spec.Model}
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

type ArchiveKind int

const (
	TarGz ArchiveKind = iota
	Zip
)

// One pinned build of an engine (llama.cpp or stable-diffusion.cpp) for one
// platform and one accelerator.
type EngineSpec struct {
	// What it is, for messages: "llama.cpp", "stable-diffusion.cpp".
	Name    string
	URL     string
	Bytes   int64
	SHA256  string
	Archive string
	Kind    ArchiveKind
	// The directory under engine/ it installs to; each backend gets its own
	// so switching accelerators never reuses the wrong build.
	DirName string
	// cpu, metal, vulkan or cuda.
	Backend Backend
	// cuda only: the oldest NVIDIA driver (major version) the build runs on.
	MinDriver int
	// Linux only: the oldest glibc the build loads against ("2.38"), set by
	// the Ubuntu release upstream built it on.
	MinGlibc string
	// Archives unpacked beside Binary after the main one — the CUDA runtime
	// ships apart from the build.
	Parts []EnginePart
	// The binaries inside the unpacked archive, relative to its root.
	Binary string
	Bench  string
	TTS    string
}

type EnginePart struct {
	URL     string
	Bytes   int64
	SHA256  string
	Archive string
	Kind    ArchiveKind
}

type Backend string

const (
	CPU    Backend = "cpu"
	Metal  Backend = "metal"
	Vulkan Backend = "vulkan"
	CUDA   Backend = "cuda"
)

// Everything the install downloads.
func (e *EngineSpec) TotalBytes() int64 {
	total := e.Bytes
	for _, part := range e.Parts {
		total += part.Bytes
	}
	return total
}

// What the install receipt records: every archive digest, so a build is
// only "installed" when all of its parts are.
func (e *EngineSpec) Receipt() string {
	digests := []string{e.SHA256}
	for _, part := range e.Parts {
		digests = append(digests, part.SHA256)
	}
	return strings.Join(digests, " ")
}

var models = []Spec{
	{
		ID:      "kev-0.6b",
		Maker:   "Kev",
		Params:  "0.6B",
		Name:    "Kev 0.6B",
		Summary: "Typed questions, calibrated probabilities; fastest kev.",
		Kind:    Decision,
		Runtime: Kev,
		Model: Pin{
			File:     "kev-0.6b-qwen3.tar.gz",
			Revision: "kev-family",
			Bytes:    43_262_595,
			SHA256:   "baf114336f20d5c21584d34cf516055b9d0cd7bc91634366a7fecb3f6cfd78fb",
			URL:      "https://github.com/jaredpalmer/kev/releases/download/kev-family/kev-0.6b-qwen3.tar.gz",
		},
		FitBytes: 3 * gib,
		Port:     7341,
	},
	{
		ID:      "kev-4b",
		Maker:   "Kev",
		Params:  "4B",
		Name:    "Kev 4B",
		Summary: "Best accuracy per byte of the kev family; the one to start with.",
		Kind:    Decision,
		Runtime: Kev,
		Model: Pin{
			File:     "kev-4b-qwen3.tar.gz",
			Revision: "kev-family",
			Bytes:    131_245_471,
			SHA256:   "01d3dc8eeccb4518e7053d94ce890d8ba7edbfa7d1c1687681c520dd1cde7769",
			URL:      "https://github.com/jaredpalmer/kev/releases/download/kev-family/kev-4b-qwen3.tar.gz",
		},
		DType:    "bf16",
		FitBytes: 10 * gib,
		Port:     7342,
	},
	{
		ID:      "kev-8b",
		Maker:   "Kev",
		Params:  "8B",
		Name:    "Kev 8B",
		Summary: "Sharpest kev answers; wants a bigger machine.",
		Kind:    Decision,
		Runtime: Kev,
		Model: Pin{
			File:     "kev-8b-qwen3.tar.gz",
			Revision: "kev-family",
			Bytes:    173_634_257,
			SHA256:   "4dd002a09f61de311a6ecea8f8e9b343b5a647a2f4bb4f0f0d24bc8a79c0d4a6",
			URL:      "https://github.com/jaredpalmer/kev/releases/download/kev-family/kev-8b-qwen3.tar.gz",
		},
		DType:    "bf16",
		FitBytes: 18 * gib,
		Port:     7343,
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
		Model: Pin{
			File:     "model.safetensors",
			Revision: "1c5edc17a7acd8701df6fc341c0d179f1c62c982",
			Bytes:    842_609_210,
			SHA256:   "891102d372688fc2a094dac56a384bc537b87c63f21f9f3dac0be2b7cbc8d86c",
		},
		Companions: []Pin{
			{File: "rl_agent_config.json", Revision: "1c5edc17a7acd8701df6fc341c0d179f1c62c982",
				Bytes: 745, SHA256: "ae287b56bbcf5f8c4f4541ae9dfd00c914c4c48b940b8398c3058af37ba92bbd"},
			{File: "encoder/config.json", Revision: "1c5edc17a7acd8701df6fc341c0d179f1c62c982",
				Bytes: 2083, SHA256: "bf3ab80598fdccf414855a2ce80f22859e4492d06ca8a62ddd1cfb63972f8979"},
			{File: "tokenizer/tokenizer.json", Revision: "1c5edc17a7acd8701df6fc341c0d179f1c62c982",
				Bytes: 3_583_228, SHA256: "6c8aaa9a542084f2457eab775d4eeb51f92a70c0fd9de28d5edb0ddec3c08d30"},
			{File: "tokenizer/tokenizer_config.json", Revision: "1c5edc17a7acd8701df6fc341c0d179f1c62c982",
				Bytes: 308, SHA256: "50044de60daaa73df97d262e15a40d4faf0160e7d742df64b377877a1320dd12"},
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
		Model: Pin{
			File:     "multilingual/model.safetensors",
			Revision: "1c5edc17a7acd8701df6fc341c0d179f1c62c982",
			Bytes:    643_835_514,
			SHA256:   "9d628fd971b700382ac6f65920a86f149777b2e748e0c955fb3b19695aa8f204",
		},
		Companions: []Pin{
			{File: "multilingual/rl_agent_config.json", Revision: "1c5edc17a7acd8701df6fc341c0d179f1c62c982",
				Bytes: 472, SHA256: "25061739243b617ad88d1219ba6f8a9c86c5881ca28df024fa2d9b3b2fcc30c6"},
			{File: "multilingual/encoder/config.json", Revision: "1c5edc17a7acd8701df6fc341c0d179f1c62c982",
				Bytes: 1938, SHA256: "83f6916d13ef0f556ac461f28308dc2bffa7ebeadee8ec9e2db5812020ea5bb4"},
			{File: "multilingual/tokenizer/tokenizer.json", Revision: "1c5edc17a7acd8701df6fc341c0d179f1c62c982",
				Bytes: 34_363_188, SHA256: "609d8f4c067cd3950f88594c5a802616cea245823836ef5848ee4fc40aab5b6f"},
			{File: "multilingual/tokenizer/tokenizer_config.json", Revision: "1c5edc17a7acd8701df6fc341c0d179f1c62c982",
				Bytes: 524, SHA256: "6c6b2d8e3c84ce0e671c129cd6b374b235d6f9863042a5836358d00a89bbb5a1"},
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
		Model: Pin{
			File:     "typed-decisions/model.safetensors",
			Revision: "1c5edc17a7acd8701df6fc341c0d179f1c62c982",
			Bytes:    842_609_220,
			SHA256:   "4fa56de72383a9d3efa9cfa78955733c81b9fc8067a587ca4beb82c78107a24e",
		},
		Companions: []Pin{
			{File: "typed-decisions/rl_agent_config.json", Revision: "1c5edc17a7acd8701df6fc341c0d179f1c62c982",
				Bytes: 847, SHA256: "ebf0cd524d92342a6be5e48e9fca3d7c2babfb5a56ccd79d2171ef5d8c7f7be8"},
			{File: "typed-decisions/encoder/config.json", Revision: "1c5edc17a7acd8701df6fc341c0d179f1c62c982",
				Bytes: 2084, SHA256: "5268d24ad3b77c8151de5dcb0762ba4391619aad9ab0bda33e36fb083cfeae6d"},
			{File: "typed-decisions/tokenizer/tokenizer.json", Revision: "1c5edc17a7acd8701df6fc341c0d179f1c62c982",
				Bytes: 3_583_228, SHA256: "6c8aaa9a542084f2457eab775d4eeb51f92a70c0fd9de28d5edb0ddec3c08d30"},
			{File: "typed-decisions/tokenizer/tokenizer_config.json", Revision: "1c5edc17a7acd8701df6fc341c0d179f1c62c982",
				Bytes: 337, SHA256: "08d4cf3ac4dca381759441b85b91a6d40e688471dcd33d15d6649eb0a9a854d1"},
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

// Every platform's pinned llama.cpp builds, the plain one first — `fornax
// pins` audits all of them, not just the local one. Sizes and digests are
// GitHub's own asset digests for b11060.
var engines = func() map[string][]*EngineSpec {
	const base = "https://github.com/ggml-org/llama.cpp/releases/download/b11060/"
	tarball := func(name string, backend Backend, bytes int64, sha256 string) *EngineSpec {
		return &EngineSpec{
			Name:    "llama.cpp",
			URL:     base + "llama-b11060-bin-" + name + ".tar.gz",
			Bytes:   bytes,
			SHA256:  sha256,
			Archive: "llama-b11060-bin-" + name + ".tar.gz",
			Kind:    TarGz,
			DirName: EngineDir(EngineVersion, backend),
			Backend: backend,
			Binary:  "llama-b11060/llama-server",
			Bench:   "llama-b11060/llama-bench",
			TTS:     "llama-b11060/llama-tts",
		}
	}
	zipball := func(name string, backend Backend, bytes int64, sha256 string) *EngineSpec {
		return &EngineSpec{
			Name:    "llama.cpp",
			URL:     base + "llama-b11060-bin-" + name + ".zip",
			Bytes:   bytes,
			SHA256:  sha256,
			Archive: "llama-b11060-bin-" + name + ".zip",
			Kind:    Zip,
			DirName: EngineDir(EngineVersion, backend),
			Backend: backend,
			Binary:  "llama-server.exe",
			Bench:   "llama-bench.exe",
			TTS:     "llama-tts.exe",
		}
	}
	cuda := func(eng *EngineSpec, minDriver int, part EnginePart) *EngineSpec {
		eng.MinDriver = minDriver
		part.URL = base + part.Archive
		eng.Parts = []EnginePart{part}
		return eng
	}
	glibc := func(eng *EngineSpec, version string) *EngineSpec {
		eng.MinGlibc = version
		return eng
	}
	return map[string][]*EngineSpec{
		"darwin/arm64": {tarball("macos-arm64", Metal, 11_178_367, "f38d330eb9e097316cbda7838d99d5414ab4195f8f96b303949ec80547168592")},
		"darwin/amd64": {tarball("macos-x64", CPU, 11_216_404, "c3799ae5495069f6d54be6dd0835a487b28f66868a83a52f542aa65a7e711dc9")},
		// Upstream builds the x64 CPU and Vulkan tarballs on Ubuntu 22.04 and
		// everything else on 24.04 (release.yml at b11060); a 24.04 build
		// fails to load on 22.04 with "GLIBC_2.38 not found".
		"linux/amd64": {
			glibc(tarball("ubuntu-x64", CPU, 16_877_346, "0ef19058e60555e9a318baa9d8490335505e6207dafdb936fb07f9623368ec46"), "2.35"),
			// CUDA 12.8 rather than 13.x: it runs on drivers from 570 up.
			glibc(cuda(tarball("ubuntu-cuda-12.8-x64", CUDA, 168_841_771, "cf454c2dac2931f18fc4146ea68c8bf3a9e86c7daa6985e93013e7a8025513f7"), 570,
				EnginePart{Archive: "cudart-llama-b11060-bin-ubuntu-cuda-12.8-x64.tar.gz", Kind: TarGz, Bytes: 594_373_580, SHA256: "b55cfd65833f4f45fa2187d4d8542772ceef6e5332138669739b3e008150233d"}), "2.38"),
			glibc(tarball("ubuntu-vulkan-x64", Vulkan, 30_384_959, "b7e4619e115b77cd8c2d1280c19eee1e3c38d2dbb559713646525ae2cdb47ca2"), "2.35"),
		},
		"linux/arm64": {
			glibc(tarball("ubuntu-arm64", CPU, 13_498_203, "3c268f1a25f2c658eaf0c8b086bcfde9d1f40221802e87ae6dc2ae849b72daf1"), "2.38"),
			glibc(cuda(tarball("ubuntu-cuda-13.3-arm64", CUDA, 145_086_312, "8780a4612d3d769138f486c2293e679329134bd22d85b0eb8e959c61f9f2f120"), 580,
				EnginePart{Archive: "cudart-llama-b11060-bin-ubuntu-cuda-13.3-arm64.tar.gz", Kind: TarGz, Bytes: 518_393_016, SHA256: "49e64bdc2c8a8df4fe5ca1ca5ac08468e44c103e9b3bd167f7732eaa22d29b5d"}), "2.38"),
			glibc(tarball("ubuntu-vulkan-arm64", Vulkan, 24_336_208, "a2d9aa237023edf0a62eb78d29c3d731b9715b3a09df35a56f09feaf224926ca"), "2.38"),
		},
		"windows/amd64": {
			zipball("win-cpu-x64", CPU, 18_463_147, "d0393b195149c4042349f8103cd6c7519546c71de4288a8a149de4c55366354b"),
			cuda(zipball("win-cuda-12.4-x64", CUDA, 254_222_353, "baad8a4e4f083165aac446c3ad0d32d438980e6ece5a3a0a69cfb45b7b262916"), 551,
				EnginePart{Archive: "cudart-llama-bin-win-cuda-12.4-x64.zip", Kind: Zip, Bytes: 391_443_627, SHA256: "8c79a9b226de4b3cacfd1f83d24f962d0773be79f1e7b75c6af4ded7e32ae1d6"}),
			zipball("win-vulkan-x64", Vulkan, 31_848_012, "a5e9dee3b0c6c688080b5489efcf9c4162c354b1e8a76288f5391e99c91d6d40"),
		},
		"windows/arm64": {zipball("win-cpu-arm64", CPU, 12_013_604, "dc07b5313dfdee239a7e06a58b766b1811da9102f9f46e59865c28e02d242e2a")},
	}
}()

// The CPU and Metal builds keep the bare version as their directory, so an
// install from before backends existed stays valid.
func EngineDir(version string, backend Backend) string {
	if backend == CPU || backend == Metal {
		return version
	}
	return version + "-" + string(backend)
}

// This platform's llama.cpp builds, the plain one first; nil when there are none.
func EngineVariants() []*EngineSpec {
	return engines[runtime.GOOS+"/"+runtime.GOARCH]
}

// Engines is every platform's pinned llama.cpp builds, keyed "goos/goarch".
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
