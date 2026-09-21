# Contributing

fornax is a single pure-stdlib Go binary — no dependencies, no build tags
beyond the per-platform memory probes. Keep it that way.

## Build and check

```sh
go build -o fornax .
gofmt -l .        # must print nothing
go vet ./...
go test ./...
```

CI runs all of that plus a six-target cross-build (`darwin/arm64`,
`darwin/amd64`, `linux/amd64`, `linux/arm64`, `windows/amd64`,
`windows/arm64`). Check it locally before pushing:

```sh
for t in darwin/arm64 darwin/amd64 linux/amd64 linux/arm64 windows/amd64 windows/arm64; do
  CGO_ENABLED=0 GOOS=${t%/*} GOARCH=${t#*/} go build -o /dev/null . || exit 1
done
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

## Adding a catalog model

Pick a public GGUF (or safetensors for `draw`) on Hugging Face, then pin it:

```sh
curl -sI "https://huggingface.co/<repo>/resolve/main/<file>"
# x-linked-etag  → sha256     x-linked-size → bytes     x-repo-commit → revision
```

Add a `modelSpec` to `models` in `catalog.go` with a unique port
(7331+ catalog, 7401+ reserved for customs). `TestEveryPinIsComplete` and
`TestModalityNeedsAProjector` will audit the entry.

## Map

| File | Owns |
| --- | --- |
| `catalog.go` | pinned models, engines, runtime kinds, fit math |
| `main.go` | dispatch, flags, `list`/`doctor`/`run`/`connect`/`rm` |
| `serve.go` | download→verify→install, `llama-server` spawn, readiness |
| `use.go` | `withServer` reuse-or-ephemeral, `ask`/`chat`/`see`/`hear`/`test`/`bench`, `ps`, `clean` |
| `kev.go` | kev runtime: uv venv, `/v1/systemone`, `judge` |
| `apple.go` + `bridge.swift` | Apple Foundation Models: Swift stdio bridge + loopback adapter |
| `draw.go` | stable-diffusion.cpp engine + `draw` |
| `say.go` | `say` — text → WAV via `llama-tts` in the llama engine |
| `talk.go`, `record.go` | `talk` (transcribe→answer→speak), `record` (mic → WAV) |
| `rerank.go` | `rerank` — `/v1/rerank` client + the reranker spec |
| `show.go` | `show` — GGUF/safetensors header reader |
| `search.go` | `search` — Hugging Face GGUF repo search |
| `version.go` | `version`/`upgrade` — release ldflags stamp |
| `mcp.go` | `mcp` — newline JSON-RPC MCP server on stdio |
| `completion.go` | `completion` — zsh/bash/fish scripts |
| `embed.go` | `/v1/embeddings` client + `embed` |
| `hf.go` | `pull hf:…` custom models + `custom.json` |
| `ollama.go` | `pull ollama:…` via the Ollama registry |
| `compare.go`, `schema.go` | `compare`; `ask --json/--schema` |
| `client.go` | OpenAI-compatible chat/embeddings client, media parts |
| `paths.go` | `~/.fornax` layout, receipts, config, API key |
| `ui.go` + `ui_*.go` | ANSI styling, spinner, TTY detection |

## Bugs and security

Open an issue with the command, the error text and `fornax doctor` output.
For security reports use GitHub's private vulnerability reporting instead of
a public issue.
