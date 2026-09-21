# fornax

A workbench for local models: download, run, talk to, see with, listen with,
benchmark and clean up — one command at a time, on any machine. Connects to
[Earshot](https://earshot.run) when it's there; fully useful when it isn't.

```sh
fornax ask qwen3-4b "explain a doorbell in one sentence"
fornax ask qwen3-4b --schema answer.json "where is the Eiffel Tower?"
fornax see qwen2.5-vl-3b screenshot.png "what does this UI say?"
fornax hear ultravox-1b take.wav
fornax draw sdxl-turbo "a tiny doorbell icon, flat style" -o icon.png
fornax embed nomic-embed "text to search over"
fornax compare qwen3-4b,apple-fm "explain a doorbell in one sentence"
fornax judge kev-4b --state "charged twice, order never arrived" \
  --ask 'escalate|noul|Needs urgent human attention?' \
  --ask 'team|choice|Which team?|returns|shipping|billing'
```

Every command fetches what it needs — a pinned llama.cpp build plus pinned
GGUF weights (and a projector file for vision/audio models) — verifies it,
and starts a loopback server if one isn't already running.

## Commands

**Get models**

| Command | What it does |
| --- | --- |
| `fornax list` | Catalog: sizes, modality, fit on this machine, what is installed |
| `fornax pull <model>` | Download a model; resumes interrupted downloads. `pull hf:Org/Repo/File.gguf` adds any public GGUF (flags: `--as`, `--kind vision\|audio --mmproj F`, `--rev`) |
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
| `fornax embed <model> [text]` | Turn text into a vector — one-line JSON on stdout |
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

`use` commands reuse the model's server when it's already running via `run`;
otherwise they spawn a temporary one on a scratch port and reap it when done.
`run` flags: `-port N`, `-ctx-size N` (default 16384), `-no-connect`.

## Install

```sh
go install github.com/earshot-run/fornax@latest
```

Or grab a release binary for macOS (arm64, x86_64), Linux (x86_64, arm64), or
Windows (x86_64, arm64). Pure stdlib, one static binary, no dependencies.

## How it connects to Earshot

When `run` is serving, fornax reads the daemon's private control
capability (`~/.earshot/control.json`, same-user only) and POSTs the server's
loopback URL to `/v1/local-models/manage`. Earshot probes `/v1/models` itself
and offers the model in the agent's picker. Older daemons without that route,
or no daemon at all, get a copy-paste block instead — connecting in Settings
never needs a token.

## What it trusts

Every artifact is pinned in source: the llama.cpp release and each GGUF (plus
each `mmproj` projector) carry an immutable revision, exact byte count, and
SHA-256. Downloads resume through `.part` files, verify before install, and
weights re-hash before every spawn. `llama-server` starts with a scrubbed
environment and requires a generated loopback API key
(`~/.fornax/server.key`, mode 600).

## Layout

```
~/.fornax/
  engine/b11060/…        one unpacked llama.cpp release
  kev/src/…              pinned kev source + its uv venv
  models/<id>/<file>     weights + projectors + verified.sha256
  models/<id>/*.part     resumable downloads
  server.key             loopback API key
  config.json            settings
```

`FORNAX_HOME` overrides the home directory.

## Catalog

| id | kind | size | notes |
| --- | --- | --- | --- |
| `qwen3-1.7b` | text | 1.7 GB | fastest replies; modest hardware |
| `qwen3-4b` | text | 2.3 GB | a small local agent model for ordinary Macs |
| `qwen3-8b` | text | 4.7 GB | sharper answers on 16 GB or more |
| `qwen3-14b` | text | 8.4 GB | the sharpest local take; wants 24 GB or more |
| `qwen2.5-vl-3b` | vision | 2.6 GB | reads screenshots, photos and documents |
| `ultravox-1b` | audio | 2.0 GB | hears audio takes; transcribes and answers |
| `kev-0.6b` | decision | ~3 GB | typed questions → probabilities; fastest kev |
| `kev-4b` | decision | ~10 GB | best accuracy per byte; the kev to start with |
| `kev-8b` | decision | ~18 GB | sharpest kev answers; wants a bigger machine |
| `apple-fm` | text | os | Apple's on-device model; needs Apple Silicon on macOS 26+ |
| `nomic-embed` | embed | 80 MB | text → vectors for search and RAG |
| `sdxl-turbo` | image | 6.5 GB | text to image in a few steps (stable-diffusion.cpp) |

Text models are `Qwen/Qwen3-*-GGUF`; vision is `ggml-org/Qwen2.5-VL-3B` and
audio is `ggml-org/ultravox-v0_5-llama-3_2-1b` — all pinned revisions on
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
x86_64 and Windows x86_64 only. `nomic-embed` is served by the same pinned
llama.cpp with `--embeddings`, so `run`/`ps`/`connect` work on it too.

Custom models: `fornax pull hf:Org/Repo/File.gguf` (or paste a
huggingface.co blob/resolve URL) resolves the pin at fetch time — the LFS
sha256 and byte size come from HF's own headers — saves the entry to
`~/.fornax/custom.json` and installs through the same verify path. The id
sticks for `ask`/`run`/`rm`/`clean` like any catalog model; `--as` names it,
`--kind vision|audio --mmproj <file>` adds a projector.

## License

MIT. llama.cpp is MIT (ggml-org); Qwen3 weights are Apache-2.0 (Qwen);
Qwen2.5-VL is Apache-2.0; Ultravox/Llama-3.2 under their model licenses;
kev is Apache-2.0 (jaredpalmer).
