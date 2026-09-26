# Security

fornax downloads and runs binaries and model weights. Every artifact is
pinned in source (immutable revision, byte count, SHA-256), verified before
install, and re-verified before every spawn. Servers bind 127.0.0.1 and
require the generated key in `~/.fornax/server.key`.

Report vulnerabilities privately through GitHub's "Report a vulnerability"
flow on this repository, not a public issue. Worth reporting: a download
path that skips verification, an archive-extraction escape, a listener off
loopback, or environment leaking into a child process.
