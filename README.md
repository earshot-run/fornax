# fornax

[![ci](https://github.com/earshot-run/fornax/actions/workflows/ci.yml/badge.svg)](https://github.com/earshot-run/fornax/actions/workflows/ci.yml)
[![license: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

A workbench for local models: download, run, talk to, see with, listen with,
imagine, embed, benchmark and clean up — one command at a time, on any machine.
Connects to [Earshot](https://earshot.run) when it's there; fully useful
when it isn't.

```sh
fornax ask hf:Qwen/Qwen3-4B-GGUF "explain a doorbell in one sentence"
fornax see hf:ggml-org/Qwen3-VL-2B-Instruct-GGUF screenshot.png "what does this UI say?"
fornax hear hf:ggml-org/ultravox-v0_5-llama-3_2-1b-GGUF take.wav
fornax imagine my-image "a tiny doorbell icon, flat style" -o icon.png
fornax embed hf:nomic-ai/nomic-embed-text-v1.5-GGUF "text to search over"
fornax compare hf:Qwen/Qwen3-4B-GGUF,apple-fm "explain a doorbell in one sentence"
fornax judge kev-4b --state "charged twice, order never arrived" \
  --ask 'escalate|noul|Needs urgent human attention?' \
  --ask 'team|choice|Which team?|returns|shipping|billing'
```

There is no model catalog to curate or keep fresh — any public Hugging Face
GGUF repo or Ollama model is a model: `hf:Org/Repo` picks a sensible quant,
`hf:Org/Repo/File.gguf` an exact one. The pin (revision, bytes, SHA-256) is
resolved at first fetch and saved to `~/.fornax/custom.json`; every command
fetches what it needs — a pinned engine (llama.cpp, stable-diffusion.cpp,
kev's source) plus those pinned weights — verifies it, and starts a loopback
server if one isn't already running. `apple-fm` needs no download at all:
it's the model inside macOS.

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
reports. While the repo is private the installer and `upgrade` use a
logged-in `gh` for auth — anonymous curl works once it's public. Shell
completions: `fornax completion zsh|bash|fish`.

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
stops it cleanly. `pull --events` emits the same `progress`, then `installed`.

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
| `kev-0.6b` / `kev-4b` / `kev-8b` | decision | typed questions → probabilities; ~3/10/18 GB |
| `laya` / `laya-multilingual` / `laya-typed-decisions` | decision | Convai's encoder decision models; ~1 GB each |
| `apple-fm` | text | Apple's on-device model; Apple Silicon on macOS 26+ |

kev models are [jaredpalmer/kev](https://github.com/jaredpalmer/kev) — a
Jev-style decision model (LoRA + readout head on Qwen3 base) that answers
typed questions with calibrated probabilities over TypeSafe's
`/v1/systemone` API instead of chatting. The pinned checkpoint tarball and a
pinned source tarball are fetched like every other artifact; a `uv` venv is
built once under `~/.fornax/kev/` (needs [uv](https://docs.astral.sh/uv/),
macOS or Linux). The Qwen3 base model downloads from Hugging Face on first
serve. `run kev-4b` prints a TypeSafe-SDK `base_url` block — the official
`typesafe-sdk` works against it unchanged.

laya models are [Convai Innovations'](https://github.com/NandhaKishorM/laya)
open-weights decision models — the same typed-question protocol as kev, but a
plain encoder forward pass instead of a LoRA'd LLM, so answers land in tens
of milliseconds. Each checkpoint's files (safetensors, `rl_agent_config.json`,
encoder and tokenizer configs) are pinned from the `convaiinnovations/laya`
HF repo, and a `uv` venv is built once under `~/.fornax/laya/` (needs
[uv](https://docs.astral.sh/uv/), macOS or Linux). The pypi package is a
library with no server, so fornax serves it through an embedded stdlib shim
(`laya_serve.py`) on loopback behind the generated key — `run laya` prints a
TypeSafe-SDK `base_url` block like kev's, but with the real `api_key`.

`judge` flags: `--state` the document, repeatable `--ask
'id|type|instructions|options…'` (types `noul`, `choice`, `score`), or
`--json request.json` for a raw SystemOne body (`-` reads stdin).

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

`fornax pull ollama:<name>[:<tag>]` (or an ollama.com/library URL) resolves
against the Ollama registry: the manifest's layer digest is already the
weights' SHA-256, so the pin comes straight from the manifest. Models with a
projector layer (llava-style vision) install it as the `mmproj` and land as
`vision` kind automatically; `--as`/`--kind` override.

## License

MIT. llama.cpp is MIT (ggml-org); Qwen, gpt-oss, Ministral and Ultravox
weights are Apache-2.0; Gemma weights are under the Gemma Terms of Use;
Llama-3.2 under its model license; kev is Apache-2.0 (jaredpalmer); laya is
Apache-2.0 (Convai Innovations).
