# Studio improvements

Make the path from a fresh install to a useful local model short, clear and
recoverable. Keep the single Go binary, embedded frontend, shared model runtime,
loopback authentication and current upstream engine resolution.

## First pass

- Give Models a clear welcome, readable cards, installed models near the top,
  memory-fit filtering and layouts that work on phones.
- Add a browser-saved Auto/Light/Dark appearance control.
- Offer live Hugging Face discovery by downloads or latest repository update;
  show update dates without treating recency as a quality score.
- Select the actual model when Open is clicked, across all four studio modes.
- Cancel stale searches and distinguish service errors from an empty result.
- Recover download buttons after failed requests, keep downloads reachable
  across filters, and preserve the requested model type on retry.
- Prevent delayed chat restoration from overriding a newly selected model.

Implemented in this pass. Backend tests use an HTTP fixture; browser regression
checks exercise the embedded frontend in Chromium. Live model downloads and
inference are separate opt-in checks.

## Next increments

1. **Choose before downloading.** Expose exact quantization and file choices,
   account for companion files in size previews, and let users inspect the
   selected recipe before installing. Validate with split weights, projectors
   and image/video companions.
2. **Explain readiness.** Surface platform/runtime support and actionable
   prerequisites beside each pick, then test first download, model startup and
   one useful result for representative chat/image/voice models on supported
   hardware. Verify current upstream files and engine assets before changing
   recommendations.
3. **Recover across restarts.** Persist resumable download intent and expose
   progress on reconnect, while distinguishing interrupted work from completed
   installs. Test process shutdown during a transfer and restart without losing
   downloaded bytes or inventing completion.

Each increment should use the existing Resolve/Pull/WithServer paths and ship
with failure-path checks. Avoid introducing another model catalog or assuming
that Hugging Face popularity proves local runtime compatibility.
