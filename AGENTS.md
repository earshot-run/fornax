# Working in this repository

fornax is a pure-stdlib Go CLI — no third-party modules. `package main`
holds the commands and the runtimes they drive; `internal/` (catalog,
download, engine, events, openai, paths, ui) holds the layers underneath.
Read `README.md` for the product and `CONTRIBUTING.md` for the file map and
pin recipe.

Verify before claiming done: `go build`, `gofmt -l .` (empty), `go vet ./...`,
`go test ./...`, and the six-target cross-build loop in CONTRIBUTING.md —
`make verify` and `make matrix` are exactly what CI runs. One test at a time
is `go test -run TestName ./` (most tests live in `package main`).
Live-model tests are opt-in via `FORNAX_LIVE=1` / `FORNAX_LIVE_DRAW=1` —
never make them run by default. A test that writes to disk sets
`t.Setenv("FORNAX_HOME", t.TempDir())`; none may touch the real `~/.fornax`.

Invariants not to break:

- Every downloaded artifact is pinned (revision + bytes + sha256) and
  verified before install; model weights re-hash before every spawn.
- Servers bind 127.0.0.1 only and require the generated key; child
  processes get a scrubbed environment.
- stdout carries only the payload — status/spinners/progress go to stderr;
  styling only when stderr is a TTY (`NO_COLOR`/`TERM=dumb` respected).
- Platform-specific behavior fails clean at resolve time; the six-target
  build matrix must keep compiling, including platforms a given runtime
  doesn't support.

## How the pieces fit

**One path through everything.** Every command that needs a model runs
`resolve()` (pull.go) → `pull()` → either `withServer()` (use.go, ephemeral)
or `serveModel()` (serve.go, the `run` case). `resolve` maps an arg to a
`catalog.Spec` — a built-in, a saved id, or a fresh `hf:`/`ollama:` ref that
gets pinned into `custom.json` on the spot — and returns the `EngineSpec`
that will serve it, failing there when this OS/arch can't run that runtime.
A new command slots into this chain rather than starting its own.

**`catalog.Runtime` is the fork.** Llama (pinned `llama-server`), Kev and
Laya (python under `uv`, both speaking `/v1/systemone`), Apple (Swift stdio
bridge fronted by a Go loopback adapter in apple.go), SD (foreground
`sd-cli`, no server at all). `withServer` dispatches on it and every branch
ends in the same `fn(url, key)` callback, so `ask`/`see`/`hear`/`test` never
learn which runtime answered. A new runtime means a `Runtime` constant, a
`with<X>` branch, a `spawn<X>`, and the resolve-time platform check.

**The pin is the security model.** `internal/download` fetches resumably and
checks the digest, and `rehash()` re-hashes weights before *every* spawn —
the install receipt cannot authorize bytes that may have changed since.
Engines are pinned per backend too (cpu/metal/vulkan/cuda; CUDA builds carry
their runtime archive as a part), and `pickEngine` (backend.go) chooses one at
resolve time from driver files, never by running anything.
`fornax pins` (pins.go) audits all of it against upstream and exits non-zero
on drift. `scrubbedEnv()` is what children get: no `LLAMA_ARG_*`, no provider
credentials, no `DYLD_*`.

**Ports are partitioned** so a one-shot command can never disturb a `run`:
built-ins are fixed in catalog.go (7341–7354), customs get 7401+ (hf.go),
scratch servers for ephemeral commands get 7431+ (`scratchPortBase`),
`studio` sits at 7340 (`studioPort`), and `studio -on` tunnels
take 7360–7399 (`studioRemotePortBase`), the same number on both ends.

**`--events` inverts stdout.** It becomes one JSON object per line
(`internal/events`) and the server's own chatter goes to
`~/.fornax/server.log`; the 5 s `alive` heartbeat doubles as a leash, so a
served model dies with the supervisor reading it.

**Three tables must stay in sync.** `commands()` in main.go, the `usage`
const beside it, and the shell completions all read the same list —
`TestUsageAndDispatchTableAgree` enforces it. Give each command's flag set
`set.Usage = usageFunc(set, "usage: …")`.

`internal/*` must not learn about runtimes. A model whose installed-ness
depends on one (kev checkpoints, the Apple bridge) is judged in `models.go`,
not in `internal/paths`.
