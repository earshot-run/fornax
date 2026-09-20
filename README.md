# earshot-local

Browse, download, and run local models for [Earshot](https://earshot.run) — one
command to a loopback OpenAI-compatible server.

```sh
earshot-local run qwen3-4b
```

That one command fetches a pinned llama.cpp build, fetches a pinned GGUF,
serves it on `127.0.0.1` with a generated API key, and hands the URL to a
running Earshot daemon — the model shows up in Settings ▸ Local models. No
Earshot? It prints the same values to paste into any OpenAI-compatible client.

## Commands

| Command | What it does |
| --- | --- |
| `earshot-local list` | Catalog: sizes, fit on this machine, what is installed |
| `earshot-local pull <model>` | Download a model (and the engine on first run); resumes interrupted downloads |
| `earshot-local run <model>` | Serve on loopback, register with Earshot; Ctrl-C stops |
| `earshot-local connect <model>` | Register an already-running model's server with Earshot |
| `earshot-local rm <model>` | Delete a model's weights and any partial download |
| `earshot-local doctor` | What this machine can run and how Earshot connectivity looks |

`run` flags: `-port N` (default is the model's catalog port), `-ctx-size N`
(default 16384), `-no-connect` (skip the Earshot registration).

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

Every artifact is pinned in source: the llama.cpp release and each GGUF carry
an immutable revision, exact byte count, and SHA-256. Downloads resume through
`.part` files, verify before install, and weights re-hash before every spawn.
`llama-server` starts with a scrubbed environment and requires a generated
loopback API key (`~/.earshot-local/server.key`, mode 600).

## Layout

```
~/.earshot-local/
  engine/b11060/…        one unpacked llama.cpp release
  models/<id>/<file>     weights + verified.sha256 + source.json
  models/<id>/*.part     resumable downloads
  server.key             loopback API key
  config.json            settings
```

`EARSHOT_LOCAL_HOME` overrides the home directory.

## Catalog

| id | size | notes |
| --- | --- | --- |
| `qwen3-1.7b` | 1.7 GB | fastest replies; modest hardware |
| `qwen3-4b` | 2.3 GB | a small local agent model for ordinary Macs |
| `qwen3-8b` | 4.7 GB | sharper answers on 16 GB or more |
| `qwen3-14b` | 8.4 GB | the sharpest local take; wants 24 GB or more |

All four are `Qwen/Qwen3-*-GGUF` on Hugging Face at pinned revisions, served
by pinned llama.cpp `b11060`. `list` marks whether each fits your RAM.

## License

MIT. llama.cpp is MIT (ggml-org); Qwen3 weights are Apache-2.0 (Qwen).
