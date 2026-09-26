# Contributing

fornax is a single pure-stdlib Go binary — no dependencies, no build tags
beyond the per-platform memory probes. Keep it that way.

## Build and check

```sh
make build     # or: go build -o fornax .
make verify    # gofmt check + vet + tests — the pre-push gate
make matrix    # all six cross-builds (CI runs the same)
```

Live tests hit real models and downloads — they are opt-in:

```sh
FORNAX_LIVE=1 go test -run 'TestCompareLive|TestSchemaLive|TestEmbedLive'
FORNAX_LIVE_DRAW=1 go test -run TestDrawLive -timeout 30m   # pulls ~7 GB
```

## Conventions

- Every fetched artifact carries an immutable revision, exact byte count and
  SHA-256 (`filePin`/`engineSpec`). Verify before install; weights re-hash
  before every spawn.
- Status, spinners and progress go to stderr; stdout carries only the
  command's output so pipes stay clean. Styling is TTY-only — `NO_COLOR`,
  `TERM=dumb` and pipes get plain text (see `ui.go`).
- Servers bind 127.0.0.1 and require the generated key
  (`~/.fornax/server.key`). Child processes get a scrubbed environment.
- A runtime that cannot run on this OS/arch fails at `resolve()` with the
  reason, not partway through a download.

## Adding a built-in model

There is no model catalog — `hf:`/`ollama:` refs resolve and pin themselves
at first fetch into `~/.fornax/custom.json`, and `internal/catalog` holds
only what can't be expressed that way (kev, laya, apple-fm). A new built-in
should be a rare thing: it needs a runtime story a plain GGUF pull can't
tell. When one is warranted, add a `modelSpec` to `models` in `catalog.go`;
`TestEveryPinIsComplete` audits the pin. A pulled model never shares a port
— customs get 7401+.

`fornax pins` audits every pin against upstream — built-in HF `main` HEADs,
the kev-family release tag, the kev/laya source commits, and every saved
custom entry — and prints paste-ready re-pins for anything that moved. Run
it before a release; it exits non-zero on drift.

A new command goes in `commands()` in `main.go` and in the `usage` text next
to it; `TestUsageAndDispatchTableAgree` holds the two together, and the shell
completions read the same table. Give its flag set
`set.Usage = usageFunc(set, "usage: …")` so `-h` lists the flags.

## Map

| File | Owns |
| --- | --- |
| `models.go` | built-ins plus saved custom models as one list; `modelInstalled` |
| `main.go` | the `commands()` dispatch table, the usage text, `main`, did-you-mean |
| `list.go` | `list` — built-ins and saved models and `--json` |
| `pull.go` | `pull`, `resolve`, and the shared model-argument parsing |
| `rm.go` | `rm`, one id or `-all` |
| `doctor.go` | `doctor` |
| `serve.go` | `run`/`connect`, install→verify, `llama-server` spawn, readiness |
| `use.go` | `withServer` reuse-or-ephemeral, `ask`/`chat`/`see`/`hear`/`test`/`bench`, `ps`, `clean` |
| `kev.go` | kev runtime: uv venv, `kev.serve` spawn; `installSourceTree` shared with laya |
| `laya.go` + `laya_serve.py` | laya runtime: uv venv, embedded stdlib `/v1/systemone` shim |
| `judge.go` | `judge` + the `/v1/systemone` client and answer printer both runtimes share |
| `apple.go` + `bridge.swift` | Apple Foundation Models: Swift stdio bridge + loopback adapter |
| `imagine.go` | stable-diffusion.cpp engine + `imagine`/`animate` |
| `say.go` | `say` — text → WAV via `llama-tts` in the llama engine |
| `talk.go`, `record.go` | `talk` (transcribe→answer→speak), `record` (mic → WAV) |
| `rerank.go` | `rerank` — `/v1/rerank` client + the reranker spec |
| `show.go` | `show` — GGUF/safetensors header reader |
| `search.go` | `search` — Hugging Face GGUF repo search |
| `pins.go` | `pins` — the upstream audit: HF HEADs, release digests, source archives |
| `version.go` | `version`/`upgrade` — release ldflags stamp |
| `mcp.go` | `mcp` — newline JSON-RPC MCP server on stdio |
| `completion.go` | `completion` — zsh/bash/fish scripts |
| `embed.go` | `/v1/embeddings` client + `embed` |
| `hf.go` | `hf:` ref parsing, repo file pick, dynamic pinning, `custom.json` |
| `ollama.go` | `pull ollama:…` via the Ollama registry |
| `compare.go`, `schema.go` | `compare`; `ask --json/--schema` |
| `idle.go` | `run -idle` over llama-server's `/metrics` counters |

### Packages

| Package | Owns |
| --- | --- |
| `internal/catalog` | the pinned model and engine table, `Spec`/`Pin`, modality, runtime, fit math |
| `internal/paths` | the `~/.fornax` layout, receipts, config, the loopback API key |
| `internal/download` | resumable pinned fetch over HTTP Range, plus the SHA-256 check |
| `internal/engine` | installing the pinned llama.cpp release; the tar/zip unpackers kev and sd reuse |
| `internal/openai` | the chat-completions client, media parts, and the raw POST embed/rerank/kev build on |
| `internal/ui` | ANSI styling, spinner, progress, `HumanSize`, the `-h` flag table |
| `internal/events` | the `--events` JSON stream and its supervisor leash |

`package main` holds the commands and the runtimes they drive. A model that
needs a runtime to decide whether it is installed (kev checkpoints, Apple's
bridge) is judged in `models.go`, not in `internal/paths` — that keeps the
lower packages free of the runtimes.

## Bugs and security

Open an issue with the command, the error text and `fornax doctor` output.
For security reports use GitHub's private vulnerability reporting instead of
a public issue.
