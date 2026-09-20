# earshot-local

A workbench for local models: download, run, talk to, see with, listen with,
benchmark and clean up — one command at a time, on any machine. Connects to
[Earshot](https://earshot.run) when it's there; fully useful when it isn't.

```sh
earshot-local ask qwen3-4b "explain a doorbell in one sentence"
earshot-local see qwen2.5-vl-3b screenshot.png "what does this UI say?"
earshot-local hear ultravox-1b take.wav
```

Every command fetches what it needs — a pinned llama.cpp build plus pinned
GGUF weights (and a projector file for vision/audio models) — verifies it,
and starts a loopback server if one isn't already running.

## Commands

**Get models**

| Command | What it does |
| --- | --- |
| `earshot-local list` | Catalog: sizes, modality, fit on this machine, what is installed |
| `earshot-local pull <model>` | Download a model; resumes interrupted downloads |
| `earshot-local rm <model>` | Delete a model's files and any partial download |
| `earshot-local clean` | Remove interrupted downloads and stale staging (`-all` wipes everything) |
| `earshot-local doctor` | What this machine can run; engine, keys and Earshot status |

**Use models**

| Command | What it does |
| --- | --- |
| `earshot-local ask <model> [prompt]` | One prompt, one streamed answer (or pipe the prompt on stdin) |
| `earshot-local chat <model>` | Interactive conversation with history (`/exit`, `/clear`) |
| `earshot-local see <model> <image> [question]` | Ask a vision model about a png/jpg/webp/gif |
| `earshot-local hear <model> <audio> [question]` | Ask an audio model about a take; transcribes by default |
| `earshot-local test <model>` | Load it, run a prompt, report tok/s |
| `earshot-local bench <model>` | `llama-bench` on the weights (prompt/generation table) |

**Run models**

| Command | What it does |
| --- | --- |
| `earshot-local run <model>` | Serve on loopback, register with Earshot; Ctrl-C stops |
| `earshot-local ps` | Which catalog models are serving right now |
| `earshot-local connect <model>` | Register an already-running model's server with Earshot |

`use` commands reuse the model's server when it's already running via `run`;
otherwise they spawn a temporary one on a scratch port and reap it when done.
`run` flags: `-port N`, `-ctx-size N` (default 16384), `-no-connect`.

## Install

```sh
go install github.com/earshot-run/earshot-local@latest
```

Or grab a release binary for macOS (arm64, x86_64), Linux (x86_64, arm64), or
Windows (x86_64, arm64). Pure stdlib, one static binary, no dependencies.

## How it connects to Earshot

When `run` is serving, earshot-local reads the daemon's private control
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
(`~/.earshot-local/server.key`, mode 600).

## Layout

```
~/.earshot-local/
  engine/b11060/…        one unpacked llama.cpp release
  models/<id>/<file>     weights + projectors + verified.sha256
  models/<id>/*.part     resumable downloads
  server.key             loopback API key
  config.json            settings
```

`EARSHOT_LOCAL_HOME` overrides the home directory.

## Catalog

| id | kind | size | notes |
| --- | --- | --- | --- |
| `qwen3-1.7b` | text | 1.7 GB | fastest replies; modest hardware |
| `qwen3-4b` | text | 2.3 GB | a small local agent model for ordinary Macs |
| `qwen3-8b` | text | 4.7 GB | sharper answers on 16 GB or more |
| `qwen3-14b` | text | 8.4 GB | the sharpest local take; wants 24 GB or more |
| `qwen2.5-vl-3b` | vision | 2.6 GB | reads screenshots, photos and documents |
| `ultravox-1b` | audio | 2.0 GB | hears audio takes; transcribes and answers |

Text models are `Qwen/Qwen3-*-GGUF`; vision is `ggml-org/Qwen2.5-VL-3B` and
audio is `ggml-org/ultravox-v0_5-llama-3_2-1b` — all pinned revisions on
Hugging Face, served by pinned llama.cpp `b11060` (`--mmproj` loads the
projectors). `list` marks whether each fits your RAM.

## License

MIT. llama.cpp is MIT (ggml-org); Qwen3 weights are Apache-2.0 (Qwen);
Qwen2.5-VL is Apache-2.0; Ultravox/Llama-3.2 under their model licenses.
