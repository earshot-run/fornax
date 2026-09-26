# fornax reference

Everything fornax does, command by command, and how it works underneath.
The README is the short version for people; this is the long one, and it is
what agents should read.

## Commands

**Get models**

| Command | What it does |
| --- | --- |
| `fornax list` | Your models: sizes, modality, fit on this machine, what is installed |
| `fornax pull <model>` | Download a model; resumes interrupted downloads. A saved id, `hf:Org/Repo[/File.gguf]` (or `Org/Repo`), or `ollama:<name>[:<tag>]` — flags: `--as`, `--kind`, `--mmproj`, `--rev` |
| `fornax rm <model>…` | Delete a model's files and any partial download; several ids at once, `-all` for every model on disk |
| `fornax clean` | Remove interrupted downloads, stale staging and old server logs (`-all` wipes everything) |
| `fornax doctor` | What this machine can run; engine, keys and Earshot status |

**Use models**

| Command | What it does |
| --- | --- |
| `fornax ask <model> [prompt]` | One prompt, one streamed answer (or pipe the prompt on stdin). `--json` constrains to a JSON object; `--schema file\|-` pins a JSON Schema |
| `fornax chat <model>` | Interactive conversation with history (`/exit`, `/clear`) |
| `fornax see <model> <image> [question]` | Ask a vision model about a png/jpg/webp/gif |
| `fornax hear <model> <audio> [question]` | Ask an audio model about a take; transcribes by default |
| `fornax imagine <model> "prompt"` | Generate an image with a model you added — `-o`, `-image`, `-steps`, `-seed`, `-size WxH`, `-neg` |
| `fornax studio` | The browser studio: chat (text, vision and audio models), image, video and voice, plus a Models page that suggests picks for this machine, searches Hugging Face and downloads with live progress. Loopback only, behind your key; images live in `~/.fornax/studio`. `-on [user@]host` runs it on another machine (a home GPU box, a rented cloud GPU) over ssh and opens it here through a tunnel — the remote stays loopback-only, and if it has no fornax yet, run from a terminal it offers to install the same release there (install.sh, sha256-checked) |
| `fornax animate <model> "prompt"` | Generate a video clip with a model you added — the same flags plus `-frames` |
| `fornax say <model> "text"` | Speak text to a WAV — `-o out.wav` (or `-` for stdout), `-voice ref.wav` clones a voice, `-lang en\|zh\|…` |
| `fornax embed <model> [text]` | Turn text into a vector — one-line JSON on stdout |
| `fornax rerank <model> "query" <doc…>` | Score documents against a query, best first (or pipe docs on stdin) — `-n` keeps the top N |
| `fornax talk <audio>` | Transcribe a take, answer it, speak the reply — `-llm`/`-stt`/`-tts` pick the models, `-voice` clones |
| `fornax record <out.wav>` | Mic to WAV — `-d seconds`, `-r rate`; feeds `hear` and `talk` |
| `fornax compare <m1,m2,…> "prompt"` | Same prompt to several models, replies + speed side by side |
| `fornax test <model>` | Load it, run a prompt, report speed |
| `fornax bench <model>` | `llama-bench` on the weights (or median request latency for kev/apple/embed) |
| `fornax judge <model> --state "…" --ask 'id|type|question|opts…'` | Typed questions → calibrated probabilities (kev, laya / TypeSafe API) |

**Run models**

| Command | What it does |
| --- | --- |
| `fornax run <model>` | Serve on loopback, register with Earshot; Ctrl-C stops. `-idle 20m` stops an unused server; `--events` emits one JSON line per stage for supervisors |
| `fornax ps` | Which models are serving right now |
| `fornax connect <model>` | Register an already-running model's server with Earshot |

**Workbench**

| Command | What it does |
| --- | --- |
| `fornax show <model>` | Pin card + a look inside the artifact — GGUF metadata (arch, params, quant, context), safetensors header, kev checkpoint |
| `fornax search <query>` | GGUF repos on Hugging Face ranked by downloads; single-file repos print the ready `pull hf:` command |
| `fornax mcp` | MCP server on stdio — agents call ask/see/hear/embed/imagine/say/models as tools |
| `fornax version` / `upgrade` | Build stamp; check for a newer release |
| `fornax completion <zsh\|bash\|fish>` | Shell completion script on stdout |

`fornax list --local` shows only installed models; `--json` prints one JSON
object per model for scripts. `pull` and `rm` take several ids at once.

`fornax help <command>` (same as `fornax <command> -h`) prints that command's
usage line and its flags; `fornax --version` is `fornax version`.

`use` commands reuse the model's server when it's already running via `run`;
otherwise they spawn a temporary one on a scratch port and reap it when done.
`run` flags: `-port N`, `-ctx-size N` (default 16384), `-idle 20m` (stop after
that long without a request), `-no-connect`, `--events`.

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/earshot-run/fornax/main/install.sh | sh
```

The installer downloads the latest release binary for your platform (macOS
arm64/x86_64, Linux x86_64/arm64), verifies it against the release's
`sha256sums.txt`, and puts it in `~/.local/bin` (override with
`FORNAX_INSTALL`). Windows: download `fornax-windows-*.exe` from
[Releases](https://github.com/earshot-run/fornax/releases).

From source needs only a Go toolchain — pure stdlib, one static binary:

```sh
go install github.com/earshot-run/fornax@latest
```

`fornax upgrade` self-updates a release install in place; `-check` only
reports. The installer uses `gh` when it's authenticated, or downloads the
public release with curl otherwise. Shell completions:
`fornax completion zsh|bash|fish`.

## How it connects to Earshot

When `run` is serving, fornax reads the daemon's private control
capability (`~/.earshot/control.json`, same-user only) and POSTs the server's
loopback URL to `/v1/local-models/manage`. Earshot probes `/v1/models` itself
and offers the model in the agent's picker. Older daemons without that route,
or no daemon at all, get a copy-paste block instead — connecting in Settings
never needs a token.

The served endpoint answers both OpenAI (`/v1/chat/completions`) and
Anthropic (`/v1/messages`) wire formats, and the generated key works as
`Authorization: Bearer` or `x-api-key` — so Claude-flavored clients just get
`ANTHROPIC_BASE_URL=http://127.0.0.1:PORT ANTHROPIC_API_KEY=<key>` (`run`
prints the ready pair).

Earshot drives fornax the other way too. Settings ▸ Local models lists your
saved models, gets a model with one button, stops it, and starts it again
when a session picks it. It does that by running this binary, nothing more,
through the same surface any front end can use:

```sh
fornax list --json                                       # one model per line: fit, installed, serving, port
fornax run hf:Qwen/Qwen3-4B-GGUF --events -idle 20m      # one JSON event per line on stdout
```

```json
{"event":"progress","label":"hf-qwen3-4b","done":251857291,"total":2497280256}
{"event":"stage","stage":"verifying","model":"hf-qwen3-4b"}
{"event":"stage","stage":"loading","model":"hf-qwen3-4b"}
{"event":"ready","model":"hf-qwen3-4b","url":"http://127.0.0.1:7401/v1","port":7401,"earshot":"connected","detail":""}
{"event":"alive"}
{"event":"stopped","model":"hf-qwen3-4b","reason":"idle"}
```

With `--events` the model server logs to `~/.fornax/server.log`, failures end
in an `error` event, and an `alive` heartbeat every five seconds doubles as a
leash: when nothing reads stdout any more, fornax stops the model. SIGTERM
stops it cleanly. `pull --events` (saved ids, `hf:` and `ollama:` refs alike)
emits the same `progress`, then `installed` with the model's id and kind, or
`error`.

## Verification

Every artifact is pinned: engine releases and the built-in models carry an
immutable revision, exact byte count and SHA-256 in source, and every pulled
model gets the same triple at fetch time — HF serves the LFS sha256 and byte
count in its own headers, Ollama's manifest digest is the layer's SHA-256.
Downloads resume through `.part` files, verify before install, and weights
re-hash before every spawn. `fornax pins` re-checks every saved pin against
upstream.
Model servers start with a scrubbed environment, bind loopback only, and
require a generated API key (`~/.fornax/server.key`, mode 600).

## Layout

```
~/.fornax/
  engine/b11060/…            one unpacked llama.cpp release
  engine/sd-master-*/…       one unpacked stable-diffusion.cpp release
  kev/src/…                  pinned kev source + its uv venv
  laya/src/… + serve.py      pinned laya source, its uv venv, the serve shim
  models/<id>/<file>         weights + projectors + verified.sha256
  models/<id>/*.part         resumable downloads
  models/apple-fm/fm-bridge  compiled Swift bridge for apple-fm
  server.key                 loopback API key
  config.json                settings
  custom.json                models added via `pull hf:…`
```

`FORNAX_HOME` overrides the home directory.

## Downloads and Hugging Face tokens

Files over 64 MB download as up to eight parallel byte ranges written in
place, with a `.part.ranges` sidecar recording each range's progress, so an
interrupted download resumes range by range; a server that ignores `Range`
gets the single-stream path. Either way nothing installs until the byte
count and SHA-256 match the pin.

A Hugging Face token (`HF_TOKEN`, or the one saved from the studio's Models
page into `config.json`, mode 600) is sent to `huggingface.co` only, never to
the CDN it redirects to. It is what unlocks gated repos (accept the model's
terms on huggingface.co first) and raises Hugging Face's per-account request
limits; it does not change bandwidth.

Set `HF_ENDPOINT` to an HTTPS mirror origin (e.g. `HF_ENDPOINT=https://hf-mirror.com fornax pull hf:Qwen/Qwen3-4B-GGUF`) when the official Hugging Face host is slow or unreachable. This routes Hugging Face model search, repo listings, pin HEAD requests and model-weight downloads through that origin. It does not rewrite other hosts (engines, Ollama, CDNs, etc.) or saved pin URLs. Only HTTPS origins without a path, query or credentials are accepted; HTTP is allowed for loopback testing. The mirror sees requested model names and file bytes, but **never receives `HF_TOKEN` or the saved token**. Gated/private repos therefore need the official endpoint; unset `HF_ENDPOINT` for those. Pin byte counts and SHA-256 checks still apply to mirror downloads. `fornax pins` audits against the upstream canonical URLs, and token validation still uses huggingface.co. A mirror may be incomplete or out of date; choose one you trust.

## GPUs

Each engine is pinned per accelerator, and fornax picks the build from what
the machine has: Metal on Apple Silicon; CUDA when an NVIDIA driver is
present (Linux and Windows, with the CUDA runtime pinned and fetched
alongside, so no toolkit install); Vulkan for other GPUs, or an NVIDIA card
whose driver is too old for the CUDA build; the CPU build otherwise.
stable-diffusion.cpp has no Linux CUDA build upstream, so image and video on
Linux run through Vulkan. `fornax doctor` shows what was found and which
build runs; `FORNAX_BACKEND=cpu|cuda|vulkan` forces one. Each build installs
to its own `engine/` directory, so switching never reuses the wrong one.

## Models

`fornax pull hf:Org/Repo` — or just `run`/`ask` the same ref — resolves the
repo's file list and picks a quant (Q4_K_M, then Q5_K_M, Q6_K, Q4_K_XL,
Q8_0…). Name the file to be exact: `hf:Org/Repo/Model-Q8_0.gguf`. Split
archives (`-00001-of-0000N`) pull every part; a repo `mmproj-*.gguf`
projector attaches itself for vision, audio and speech models; the kind is
inferred from the name (`rerank`, `embed`, `tts`, `vl`, `asr`…) and `--kind`
overrides it. `search <query>` ranks GGUF repos by downloads.

Everything lands in `custom.json` with a derived id — `hf:Qwen/Qwen3-4B-GGUF`
becomes `hf-qwen3-4b` — which `list`/`ask`/`run`/`rm`/`clean` treat like a
built-in from then on; the same ref resolves offline once saved. `list`
marks whether each fits your RAM. Served by pinned llama.cpp `b11060`
(`--mmproj` loads projectors, `--embeddings`/`--reranking` the rest).

Built-ins — the models that can't come from a GGUF repo:

| id | kind | notes |
| --- | --- | --- |
| `kev-0.8b` / `kev-4b` / `kev-9b` / `kev-27b` | decision | typed questions → probabilities; ~4/12/24/60 GB |
| `laya` / `laya-multilingual` / `laya-typed-decisions` | decision | Convai's encoder decision models; ~1 GB each |
| `apple-fm` | text | Apple's on-device model; Apple Silicon on macOS 26+ |

Decision models answer typed questions about a piece of state with
calibrated probabilities over TypeSafe's `/v1/systemone` API instead of
chatting. Three question types: `noul` (yes/no → the probability of yes),
`choice` (one of a set → a distribution over the options) and `score`
(ordered levels, listed low to high → the expected level, its nearest
label, and the distribution). Both families need
[uv](https://docs.astral.sh/uv/) and macOS or Linux; the pinned source and
checkpoint are fetched like every other artifact, and a venv is built once.

kev models are [jaredpalmer/kev](https://github.com/jaredpalmer/kev) — a
Jev-style decision model (LoRA + readout head on a Qwen3.5 base, Qwen3.8
for 27B). Start with `kev-4b`; upstream sizes it for a 32 GB Mac or a
16 GB+ GPU, and it answers the three-question example below in ~180 ms on
an RTX 4080. The checkpoint is small (123 MB for 4B), but the first serve
downloads its base model from Hugging Face at the revision the checkpoint
names, into the shared `~/.cache/huggingface` — about 9 GB for 4B — and
the first `uv` build pulls torch, a few GB more. On CUDA fornax serves with
`KEV_FUSED=0`: upstream's default fused kernels import
flash-linear-attention, which kev's `serve` extra does not install, so the
stock server crashes there; the plain torch path answers the same, and
upstream measures fused as about a third less GPU time per batch.

laya models are [Convai Innovations'](https://github.com/NandhaKishorM/laya)
open-weights decision models on ModernBERT (mmBERT for
`laya-multilingual`) — ~1 GB each with nothing else to download, so they
are the quick install. Each checkpoint's files (safetensors,
`rl_agent_config.json`, encoder and tokenizer configs) are pinned from the
`convaiinnovations/laya` HF repo. The pypi package is a library with no
server, so fornax serves it through an embedded stdlib shim
(`laya_serve.py`). On the support-message example laya answered in
~0.7 s warm and gave "needs a human" 0.07 where kev-4b gave 0.94; use kev
when the answer matters.

Both serve on loopback behind the generated key: `run kev-4b` (or `run
laya`) prints a TypeSafe-SDK block — `base_url`, `model`, `api_key` — and
the official `typesafe-sdk` works against it unchanged. kev also answers
as `jev-latest`, the SDK's default model name.

`judge` flags: `--state` the document, repeatable `--ask
'id|type|instructions|options…'` (types `noul`, `choice`, `score`), or
`--json request.json` for a raw SystemOne body (`-` reads stdin). `judge`
reuses a model already up under `fornax run`; otherwise it starts one for
the call and stops it after, which reloads the base each time (~15 s for
kev-4b on the 4080), so keep `run` up when judging in a loop.

```
fornax judge kev-4b --state "Customer: I was charged twice this month…" \
  --ask 'escalate|noul|Does this need a human today?' \
  --ask 'topic|choice|What is it about?|billing|bug|sales|other' \
  --ask 'mood|score|How upset is the customer?|calm|annoyed|angry'
```

`apple-fm` is Apple's Foundation Models framework — the ~3B model that ships
inside macOS 26+ on Apple Silicon, so there is nothing to download. `pull`
compiles an embedded Swift bridge (`bridge.swift`, JSONL over stdio) and
fornax fronts it with a loopback adapter that speaks the same
OpenAI-compatible API as the other models — `ask`, `chat`, `test`, `bench`,
`run`, `ps` and `connect` all work. It needs Apple Intelligence enabled in
System Settings, and `swiftc` (Xcode Command Line Tools) for the one-time
bridge compile. Token counts in `test`/`bench` are estimates (~4 chars/token)
— the framework doesn't expose them. Text only: `see`, `hear` and `judge`
don't apply.

Image and video run on a pinned stable-diffusion.cpp build (`sd-cli`,
foreground — no server) with engine binaries for macOS arm64, Linux x86_64
and Windows x86_64 only. fornax pins that engine and no image or video
model: you add the one you want, with the files and `sd-cli` arguments it
needs saved beside it, and `imagine`/`animate` run it that way every time.

```
fornax pull hf:Org/Repo/video.gguf --as my-video --kind video \
  --with vae=hf:Org/Other/vae.safetensors --with t5xxl=hf:Org/Enc/umt5.gguf \
  --args "--steps 4 --cfg-scale 1.0 --video-frames 33"
```

`--with <flag>=hf:…` is any `sd-cli` file flag. Weights that come with
companion files load as `--diffusion-model`; a lone checkpoint loads as `-m`.
Speech models run through `llama-tts` in the same pinned llama.cpp archive —
foreground, no server — writing a 24 kHz WAV per call; `-voice take.wav`
clones a voice from a reference take. Embed and rerank models are served by
the same pinned llama.cpp with `--embeddings`/`--reranking`, so `run`/`ps`/
`connect` work on them too.

Speech output and reference paths are relative to the directory where you
invoked fornax, including through MCP. The engine's working directory does
not change where those files are read or written.

`fornax pull ollama:<name>[:<tag>]` (or an ollama.com/library URL) resolves
against the Ollama registry: the manifest's layer digest is already the
weights' SHA-256, so the pin comes straight from the manifest. Models with a
projector layer (llava-style vision) install it as the `mmproj` and land as
`vision` kind automatically; `--as`/`--kind` override.
