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
FORNAX_LIVE=1 go test -run 'TestCompareLive|TestSchemaLive|TestEmbedLive' .
```

## Conventions

- Nothing is pinned. Engines come from the newest upstream release that
  carries the build, python runtimes from their repo's HEAD, models from the
  revision their ref names (main by default); downloads take their size from
  the server and are not hash-checked.
- Status, spinners and progress go to stderr; stdout carries only the
  command's output so pipes stay clean. Styling is TTY-only — `NO_COLOR`,
  `TERM=dumb` and pipes get plain text (see `internal/ui`).
- Servers bind 127.0.0.1 and require the generated key
  (`~/.fornax/server.key`). Child processes get a scrubbed environment.
- A runtime that cannot run on this OS/arch fails at `modelrt.Resolve` with
  the reason, not partway through a download.

## Adding a built-in model

There is no model catalog — `hf:`/`ollama:` refs resolve and save themselves
at first fetch into `~/.fornax/custom.json`, and `internal/catalog` holds
only what can't be expressed that way (kev, laya, apple-fm). A new built-in
should be a rare thing: it needs a runtime story a plain GGUF pull can't
tell. When one is warranted, add a `modelSpec` to `models` in `catalog.go`;
`TestEveryModelFileHasAURL` checks its files. A pulled model never shares a
port — customs get 7401+.

Engine builds are listed per platform *and* backend — `engines` in
`catalog.go` for llama.cpp, `sdEngines` in `internal/modelrt/sd.go` — plain
build first, GPU builds after, with a CUDA build's runtime archive as a
`Part`. Each names a repo and an asset-name pattern (or a container image
and the layer holding the build); `engine.Ensure` resolves it against the
newest release at install time. When upstream renames an asset, update the
pattern and the real names in `TestEngineAssetsMatchUpstreamNames` /
`TestSDAssetsMatchUpstreamNames`
(`gh api 'repos/<owner>/<repo>/releases?per_page=1' --jq '.[0].assets[].name'`).
`internal/modelrt/backend.go` picks among them.

A new command goes in `commands()` in `main.go` and in the `usage` text next
to it; `TestUsageAndDispatchTableAgree` holds the two together, and the shell
completions read the same table. Give its flag set
`set.Usage = ui.UsageFunc(set, "usage: …")` so `-h` lists the flags.

## Map

The root `package main` is the commands — flags, and what each one prints.
Everything that knows how a model is fetched, verified, spawned or served
lives in `internal/modelrt`; the commands, the studio and the MCP server
reach it only through its exported API.

### Commands (root)

| File | Owns |
| --- | --- |
| `main.go` | the `commands()` dispatch table, the usage text, `main`, did-you-mean, the `mcp` entry |
| `list.go` | `list` — built-ins and saved models and `--json` |
| `pull.go` | `pull` for saved ids, `hf:` and `ollama:` refs; `parseFlexible` and `modelArgs`, the argument parsing commands share |
| `rm.go` | `rm`, one id or `-all` |
| `doctor.go` | `doctor` |
| `run.go` | `run`/`connect` flags |
| `use.go` | `ask`/`chat`/`see`/`hear`/`test`/`bench`, `ps`, `clean` |
| `judge.go` | `judge` + the `/v1/systemone` client and answer printer both decision runtimes share |
| `imagine.go` | `imagine`/`animate` flags |
| `studio.go` | `studio` flags |
| `say.go` | `say`, and `chat -speak` playback |
| `talk.go`, `record.go` | `talk` (transcribe→answer→speak), `record` (mic → WAV) |
| `embed.go`, `rerank.go` | `embed`; `rerank` and its `/v1/rerank` client |
| `compare.go`, `schema.go` | `compare`; `ask --json/--schema` |
| `show.go` | `show` — the model card, GGUF metadata, the safetensors header |
| `search.go` | `search` — Hugging Face GGUF repo search |
| `version.go` | `version`/`upgrade` — release ldflags stamp (`-X main.version`) |
| `completion.go` | `completion` — zsh/bash/fish scripts |

### `internal/modelrt` — the model runtime

| File | Owns |
| --- | --- |
| `models.go` | built-ins plus saved custom models as one list; `Installed`, `RequireChat` |
| `pull.go` | `Resolve` (arg → spec + engine, failing clean on an unsupported platform) and `Pull` |
| `serve.go` | model install, `scrubbedEnv`, `llama-server` spawn, readiness; `Serve` (`run`) and `Connect` |
| `withserver.go` | `WithServer` — reuse-or-ephemeral across every runtime; `Bench` |
| `hf.go` | `hf:` ref parsing, repo file pick, file sizes, `custom.json` |
| `ollama.go` | `ollama:` refs via the Ollama registry |
| `kev.go` | kev runtime: uv venv, `kev.serve` spawn; `installSourceTree` shared with laya |
| `laya.go` + `laya_serve.py` | laya runtime: uv venv, embedded stdlib `/v1/systemone` shim |
| `apple.go` + `bridge.swift` | Apple Foundation Models: Swift stdio bridge + loopback adapter |
| `sd.go` | the stable-diffusion.cpp builds, `PrepareSD`, the `sd-cli` invocation |
| `tts.go` | `llama-tts`: `PrepareSpeech`, `RunSay`, `TTSCommand` |
| `backend.go` | which engine build (cpu/metal/vulkan/cuda) this machine runs |
| `earshot.go` | handing a running server to Earshot, or the block to paste |
| `idle.go` | `run -idle` over llama-server's `/metrics` counters |
| `clean.go` | `Clean` — interrupted downloads and stale staging |
| `memory_*.go` | total RAM, per OS |

### `internal/studio` and `internal/mcp`

| File | Owns |
| --- | --- |
| `studio/studio.go` + `studio/web/` | the studio server: loopback guard, queue, library; the page is embedded from `web/` |
| `studio/studio_sd.go`, `studio/studio_voice.go` | the image/video and speech kinds the queue makes |
| `studio/studio_chat.go` | chat over a warm model server |
| `studio/hub.go` | the Models page: picks (a ref + companion recipe + download size each, saved at first fetch like any `pull hf:`), Hugging Face search and size preview, downloads run as `fornax pull … --events` |
| `studio/studio_remote.go` | `studio -on host` — the studio on another machine over an ssh tunnel; the `-leash` that ends it with the connection |
| `mcp/mcp.go` | `mcp` — newline JSON-RPC MCP server on stdio |

### Packages below the runtime

| Package | Owns |
| --- | --- |
| `internal/catalog` | the built-in model and engine table, `Spec`/`Artifact`, modality, runtime, fit math |
| `internal/paths` | the `~/.fornax` layout, receipts, config, the loopback API key |
| `internal/download` | resumable fetch over HTTP Range, and the size probe |
| `internal/engine` | finding an engine build in the newest upstream release (GitHub, PyPI, container layer) and installing it; the tar/zip unpackers kev reuses |
| `internal/openai` | the chat-completions client, embeddings, media parts, the keyed GET and raw POST the other clients build on |
| `internal/gguf` | the GGUF metadata header reader behind `show` |
| `internal/ui` | ANSI styling, spinner, progress, `HumanSize`, the `-h` flag table |
| `internal/events` | the `--events` JSON stream and its supervisor leash |

These must not learn about runtimes. A model that needs a runtime to decide
whether it is installed (kev checkpoints, Apple's bridge) is judged in
`modelrt.Installed`, not in `internal/paths`.

## Bugs and security

Open an issue with the command, the error text and `fornax doctor` output.
For security reports use GitHub's private vulnerability reporting instead of
a public issue.
