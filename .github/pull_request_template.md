## What changed

## Verification

- [ ] `make verify` (gofmt, vet, tests)
- [ ] `make matrix` if platform-specific code changed
- [ ] Live-tested the changed command, if it runs a model or engine

## Pins

New artifact → immutable revision, exact bytes, SHA-256 in `catalog.go`.
