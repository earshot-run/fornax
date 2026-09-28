# Working in this repository

fornax is a pure-stdlib Go CLI — no third-party modules. The root
`package main` holds only the commands: flag parsing and what each one prints.
`internal/modelrt` is the model runtime every command drives,
`internal/studio` and `internal/mcp` are the two long-running front ends, and
`internal/` (catalog, download, engine, events, gguf, openai, paths, ui)
holds the layers underneath. Read `docs/reference.md` for every command,
flag and behavior (the README is a short pitch for people; keep it that way
and put detail in the reference), and `CONTRIBUTING.md` for the file map.

Verify before claiming done: `go build`, `gofmt -l .` (empty),
`go vet ./...`, `go test ./...`, and the six-target cross-build loop in
CONTRIBUTING.md — `make verify` and `make matrix` are exactly what CI runs.
One test at a time is `go test -run TestName ./<dir>` — a test lives beside
the code it tests, so runtime tests are in `./internal/modelrt`, studio tests
in `./internal/studio`, command tests in `./`. Live-model tests are opt-in
via `FORNAX_LIVE=1` — never make them run by default. A test that writes to
disk sets `t.Setenv("FORNAX_HOME", t.TempDir())`; none may touch the real
`~/.fornax`.

Invariants not to break:

- Nothing is pinned: engines come from the newest upstream release that
  carries the build, python runtimes from their repo's HEAD, models from the
  revision their ref names (main by default). Weights and projectors from
  Hugging Face are checked against the sha256 Hugging Face publishes (the LFS
  etag), and Ollama layers against their OCI blob digest; engine builds and
  GitHub release assets publish no digest and are not checked.
- Servers bind 127.0.0.1 only and require the generated key; child
  processes get a scrubbed environment.
- stdout carries only the payload — status/spinners/progress go to stderr;
  styling only when stderr is a TTY (`NO_COLOR`/`TERM=dumb` respected).
- Platform-specific behavior fails clean at resolve time; the six-target
  build matrix must keep compiling, including platforms a given runtime
  doesn't support.

## How the pieces fit

**One path through everything.** Every command that needs a model runs
`modelrt.Resolve` (modelrt/pull.go) → `Pull` → either `WithServer`
(modelrt/withserver.go, ephemeral) or `Serve` (modelrt/serve.go, the `run`
case). `Resolve` maps an arg to a `catalog.Spec` — a built-in, a saved id, or
a fresh `hf:`/`ollama:` ref that gets saved into `custom.json` on the spot —
and returns the `EngineSpec` that will serve it, failing there when this
OS/arch can't run that runtime. A new command slots into this chain rather
than starting its own; the studio and the MCP server reach models the same
way, through modelrt's exported API only.

**`catalog.Runtime` is the fork.** Llama (`llama-server`), Kev and
Laya (python under `uv`, both speaking `/v1/systemone`), Apple (Swift stdio
bridge fronted by a Go loopback adapter in modelrt/apple.go), SD (foreground
`sd-cli` for `imagine`/`animate`, or `sd-server` behind the keyed images
adapter in modelrt/sdserve.go when `run` serves an image model).
`WithServer` dispatches on it and every branch ends in the same `fn(url, key)`
callback, so `ask`/`see`/`hear`/`test` never learn which runtime answered. A
new runtime means a `Runtime` constant, a `with<X>` branch, a `spawn<X>`, and
the resolve-time platform check.

**Engines resolve at install time.** A `catalog.EngineSpec` names where a
build lives — a GitHub repo plus an asset-name pattern, or a container image
plus the layer holding it — never a version. `engine.Ensure` takes the newest
release that carries it, downloads it with its parts (the CUDA runtime, a
PyPI wheel) resumably through `internal/download`, and records the release in
the engine dir's `installed` file; an installed build is used until it is
removed. Each backend (cpu/metal/vulkan/cuda) gets its own dir, and
`pickEngine` (modelrt/backend.go) chooses one at resolve time from driver
files, never by running anything. `scrubbedEnv()` (modelrt/serve.go) is what
children get: no `LLAMA_ARG_*`, no provider credentials, no `DYLD_*`. Every
spawn stays inside modelrt; what leaves it is either a finished result or an
`*exec.Cmd` built there (`SDCommand`, `TTSCommand`).

**Ports are partitioned** so a one-shot command can never disturb a `run`:
built-ins are fixed in catalog.go (7341–7354), customs get 7401+
(modelrt/hf.go), scratch servers for ephemeral commands get 7431+
(`scratchPortBase`, modelrt/withserver.go), `studio` sits at 7340
(`studio.Port`), and `studio -on` tunnels take 7360–7399
(`studio.RemotePortBase`), the same number on both ends.

**`--events` inverts stdout.** It becomes one JSON object per line
(`internal/events`) and the server's own chatter goes to
`~/.fornax/server.log`; the 5 s `alive` heartbeat doubles as a leash, so a
served model dies with the supervisor reading it.

**Three tables must stay in sync.** `commands()` in main.go, the `usage`
const beside it, and the shell completions all read the same list —
`TestUsageAndDispatchTableAgree` enforces it. Give each command's flag set
`set.Usage = ui.UsageFunc(set, "usage: …")`.

**Runtimes stop at `internal/modelrt`.** The packages below it (catalog,
download, engine, events, gguf, openai, paths, ui) must not learn about
runtimes, and nothing above it — the commands, `internal/studio`,
`internal/mcp` — reaches past its exported API into how a runtime spawns
or installs. A model whose installed-ness depends on a runtime (kev
checkpoints, the Apple bridge) is judged in `modelrt.Installed`, not in
`internal/paths`.