# fornax

[![ci](https://github.com/earshot-run/fornax/actions/workflows/ci.yml/badge.svg)](https://github.com/earshot-run/fornax/actions/workflows/ci.yml)
[![license: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

A workbench for local models: download, run, talk to, see with, listen with,
draw, embed, benchmark and clean up — one command at a time, on any machine.
Connects to [Earshot](https://earshot.run) when it's there; fully useful
when it isn't.

```sh
fornax ask qwen3-4b "explain a doorbell in one sentence"
fornax ask qwen3-4b --schema answer.json "where is the Eiffel Tower?"
fornax see qwen3-vl-2b screenshot.png "what does this UI say?"
fornax hear ultravox-1b take.wav
fornax draw sdxl-turbo "a tiny doorbell icon, flat style" -o icon.png
fornax embed nomic-embed "text to search over"
fornax compare qwen3-4b,apple-fm "explain a doorbell in one sentence"
fornax judge kev-4b --state "charged twice, order never arrived" \
  --ask 'escalate|noul|Needs urgent human attention?' \
  --ask 'team|choice|Which team?|returns|shipping|billing'
```

Every command fetches what it needs — a pinned engine (llama.cpp,
stable-diffusion.cpp, kev's source) plus pinned weights (with a projector
file for vision/audio models) — verifies it, and starts a loopback server
if one isn't already running. `apple-fm` needs no download at all: it's the
model inside macOS.

## Commands

**Get models**

| Command | What it does |
| --- | --- |
| `fornax list` | Catalog: sizes, modality, fit on this machine, what is installed |
| `fornax pull <model>` | Download a model; resumes interrupted downloads. `pull hf:Org/Repo/File.gguf` or `pull ollama:<name>[:<tag>]` adds any public model (flags: `--as`, `--kind vision\|audio --mmproj F`, `--rev`) |
| `fornax rm <model>` | Delete a model's files and any partial download |
| `fornax clean` | Remove interrupted downloads, stale staging and old server logs (`-all` wipes everything) |
| `fornax doctor` | What this machine can run; engine, keys and Earshot status |

**Use models**

| Command | What it does |
| --- | --- |
| `fornax ask <model> [prompt]` | One prompt, one streamed answer (or pipe the prompt on stdin). `--json` constrains to a JSON object; `--schema file\|-` pins a JSON Schema |
| `fornax chat <model>` | Interactive conversation with history (`/exit`, `/clear`) |
| `fornax see <model> <image> [question]` | Ask a vision model about a png/jpg/webp/gif |
| `fornax hear <model> <audio> [question]` | Ask an audio model about a take; transcribes by default |
| `fornax draw <model> "prompt"` | Generate an image — `-o`, `-steps`, `-seed`, `-size WxH`, `-neg` |
| `fornax say <model> "text"` | Speak text to a WAV — `-o out.wav` (or `-` for stdout), `-voice ref.wav` clones a voice, `-lang en\|zh\|…` |
| `fornax embed <model> [text]` | Turn text into a vector — one-line JSON on stdout |
| `fornax rerank <model> "query" <doc…>` | Score documents against a query, best first (or pipe docs on stdin) — `-n` keeps the top N |
| `fornax talk <audio>` | Transcribe a take, answer it, speak the reply — `-llm`/`-stt`/`-tts` pick the models, `-voice` clones |
| `fornax record <out.wav>` | Mic to WAV — `-d seconds`, `-r rate`; feeds `hear` and `talk` |
| `fornax compare <m1,m2,…> "prompt"` | Same prompt to several models, replies + speed side by side |
| `fornax test <model>` | Load it, run a prompt, report speed |
| `fornax bench <model>` | `llama-bench` on the weights (or median request latency for kev/apple/embed) |
| `fornax judge <model> --state "…" --ask 'id|type|question|opts…'` | Typed questions → calibrated probabilities (kev / TypeSafe API) |

**Run models**

| Command | What it does |
| --- | --- |
| `fornax run <model>` | Serve on loopback, register with Earshot; Ctrl-C stops |
| `fornax ps` | Which catalog models are serving right now |
| `fornax connect <model>` | Register an already-running model's server with Earshot |

**Workbench**

| Command | What it does |
| --- | --- |
| `fornax show <model>` | Pin card + a look inside the artifact — GGUF metadata (arch, params, quant, context), safetensors header, kev checkpoint |
| `fornax search <query>` | GGUF repos on Hugging Face ranked by downloads; single-file repos print the ready `pull hf:` command |
| `fornax mcp` | MCP server on stdio — agents call ask/see/hear/embed/draw/say/list as tools |
| `fornax version` / `upgrade` | Build stamp; check for a newer release |
| `fornax completion <zsh\|bash\|fish>` | Shell completion script on stdout |

`fornax list --local` shows only installed models; `--json` prints one JSON
object per model for scripts. `pull` takes several ids at once.

`use` commands reuse the model's server when it's already running via `run`;
otherwise they spawn a temporary one on a scratch port and reap it when done.
`run` flags: `-port N`, `-ctx-size N` (default 16384), `-no-connect`.

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

## Verification

Every artifact is pinned in source: each engine release and each weights file
(plus `mmproj` projectors and `hf:` customs, pinned at add time) carries an
immutable revision, exact byte count, and SHA-256. Downloads resume through
`.part` files, verify before install, and weights re-hash before every spawn.
Model servers start with a scrubbed environment, bind loopback only, and
require a generated API key (`~/.fornax/server.key`, mode 600).

## Layout

```
~/.fornax/
  engine/b11060/…            one unpacked llama.cpp release
  engine/sd-master-*/…       one unpacked stable-diffusion.cpp release
  kev/src/…                  pinned kev source + its uv venv
  models/<id>/<file>         weights + projectors + verified.sha256
  models/<id>/*.part         resumable downloads
  models/apple-fm/fm-bridge  compiled Swift bridge for apple-fm
  server.key                 loopback API key
  config.json                settings
  custom.json                models added via `pull hf:…`
```

`FORNAX_HOME` overrides the home directory.

## Catalog

| id | kind | size | notes |
| --- | --- | --- | --- |
| `qwen3-4b` | text | 2.3 GB | a small local agent model for ordinary Macs |
| `qwen3-8b` | text | 4.7 GB | sharper answers on 16 GB or more |
| `qwen3-14b` | text | 8.4 GB | the largest model in the catalog; wants 24 GB or more |
| `qwen3.5-0.8b` | text | 795 MB | newest tiny Qwen; very fast |
| `ministral-3-8b` | text | 8.4 GB | Mistral's current 8B instruct |
| `gpt-oss-20b` | text | 11.3 GB | OpenAI's open MoE — 3.6B active, reasoning and tools |
| `gemma-4-e4b` | vision | 8.6 GB | Google's multimodal 4B — text and images |
| `qwen3.8-27b` | vision | 19 GB | current flagship local; sees images too, wants 32 GB |
| `qwen3-vl-2b` | vision | 2.1 GB | current-gen vision-language at 2B |
| `ultravox-1b` | audio | 2.0 GB | hears audio takes; transcribes and answers |
| `qwen3-asr-0.6b` | audio | 972 MB | tiny dedicated speech-to-text |
| `kev-0.6b` | decision | ~3 GB | typed questions → probabilities; fastest kev |
| `kev-4b` | decision | ~10 GB | best accuracy per byte; the kev to start with |
| `kev-8b` | decision | ~18 GB | largest kev; wants a bigger machine |
| `apple-fm` | text | os | Apple's on-device model; needs Apple Silicon on macOS 26+ |
| `nomic-embed` | embed | 80 MB | text → vectors for search and RAG |
| `embeddinggemma-300m` | embed | 318 MB | Google's small multilingual embedder |
| `qwen3-tts-1.7b` | speech | 2.1 GB | text → speech in 10 languages, voice cloning via `-voice` |
| `sdxl-turbo` | image | 6.5 GB | text to image in a few steps (stable-diffusion.cpp) |

Text spans `Qwen/Qwen3-*-GGUF`, `ggml-org/Qwen3.5-0.8B`,
`ggml-org/Ministral-3-8B-Instruct` and `ggml-org/gpt-oss-20b`; vision is
`ggml-org/Qwen3-VL-2B`, `gemma-4-E4B` and `Qwen3.8-27B`;
audio is `ggml-org/ultravox` and `Qwen3-ASR` — all pinned revisions on
Hugging Face, served by pinned llama.cpp `b11060` (`--mmproj` loads the
projectors). `list` marks whether each fits your RAM.

kev models are [jaredpalmer/kev](https://github.com/jaredpalmer/kev) — a
Jev-style decision model (LoRA + readout head on Qwen3 base) that answers
typed questions with calibrated probabilities over TypeSafe's
`/v1/systemone` API instead of chatting. The pinned checkpoint tarball and a
pinned source tarball are fetched like every other artifact; a `uv` venv is
built once under `~/.fornax/kev/` (needs [uv](https://docs.astral.sh/uv/),
macOS or Linux). The Qwen3 base model downloads from Hugging Face on first
serve. `run kev-4b` prints a TypeSafe-SDK `base_url` block — the official
`typesafe-sdk` works against it unchanged.

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

`sdxl-turbo` runs on a pinned stable-diffusion.cpp build (`sd-cli`,
foreground — no server) and ships engine binaries for macOS arm64, Linux
x86_64 and Windows x86_64 only. `qwen3-tts-1.7b` runs through `llama-tts`
in the same pinned llama.cpp archive — also foreground, no server —
writing a 24 kHz WAV per call; `-voice take.wav` clones a voice from a
reference take. `nomic-embed` is served by the same pinned llama.cpp with
`--embeddings`, so `run`/`ps`/`connect` work on it too.

Custom models: `fornax pull hf:Org/Repo/File.gguf` (or paste a
huggingface.co blob/resolve URL) resolves the pin at fetch time — the LFS
sha256 and byte size come from HF's own headers — saves the entry to
`~/.fornax/custom.json` and installs through the same verify path. The id
sticks for `ask`/`run`/`rm`/`clean` like any catalog model; `--as` names it,
`--kind vision|audio --mmproj <file>` adds a projector.

`fornax pull ollama:<name>[:<tag>]` (or an ollama.com/library URL) does the
same against the Ollama registry: the manifest's layer digest is already the
weights' SHA-256, so the pin comes straight from the manifest. Models with a
projector layer (llava-style vision) install it as the `mmproj` and land as
`vision` kind automatically; `--as`/`--kind` override.

## License

MIT. llama.cpp is MIT (ggml-org); Qwen, gpt-oss, Ministral and Ultravox
weights are Apache-2.0; Gemma weights are under the Gemma Terms of Use;
Llama-3.2 under its model license; kev is Apache-2.0 (jaredpalmer).
