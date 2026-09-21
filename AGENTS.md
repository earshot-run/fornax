# Working in this repository

fornax is a pure-stdlib Go CLI — no third-party modules. Read `README.md`
for the product and `CONTRIBUTING.md` for the file map and pin recipe.

Verify before claiming done: `go build`, `gofmt -l .` (empty), `go vet ./...`,
`go test ./...`, and the six-target cross-build loop in CONTRIBUTING.md.
Live-model tests are opt-in via `FORNAX_LIVE=1` / `FORNAX_LIVE_DRAW=1` —
never make them run by default.

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
