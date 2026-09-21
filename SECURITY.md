# Security

fornax downloads and runs binaries and model weights, so the trust model is
the product: every artifact is pinned to an immutable revision, byte count
and SHA-256 in source, verified before install, and re-verified before every
spawn. Servers bind loopback only behind a generated key.

If you find a hole in that — a download path that skips verification, an
archive-extraction escape, a listener off loopback, leaked environment into a
child process — please report it privately through GitHub's *Report a
vulnerability* flow on this repository rather than a public issue.
