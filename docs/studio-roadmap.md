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

## Model setup and recovery

Implemented after the first pass:

- Exact GGUF weight choices, hiding projectors and later split shards from the
  selector. Generic downloads check their complete file set before submitting
  the resolved reference.
- Read-only previews of every weight shard, projector and recipe companion,
  including per-file sizes, the full total and the saved engine arguments.
  Alternate quants keep their original curated recipe.
- Runtime/build support and installed-engine information beside picks and in
  previews; unsupported downloads explain why they are unavailable.
- Memory-aware selection counts shards and projectors. Revision-specific
  listings match the requested revision; incomplete split archives fail early.
- A private download journal preserves requests and progress. Shutdown stops
  active children; restarts show Resume, retaining range-resumable partial files
  through the shared downloader. Cancel dismisses intent, and stale completion
  records cannot reappear as installed models.
- Studio downloads use an input lease to stop children immediately if studio
  exits, with heartbeat cancellation for other event supervisors.
- Download completion requires an installed event. Oversized event streams
  fail without leaving a blocked child process.

Backend fixtures cover recipe totals, revisions, interrupted downloads and
journal failures. Chromium checks cover choices, inspection, readiness and
resuming the exact request, alongside the first-pass workflows.

## Storage and conversations

Implemented in the next iteration:

- File inspection shows available disk space. The shared CLI/studio downloader
  rejects known-size transfers that exceed available capacity, accounts for
  resumable bytes, and preserves partial files on failure. Capacity probes cover
  Linux, macOS and Windows without adding dependencies.
- Local conversation search across titles, models, message text and reasoning,
  with stale-search cancellation, empty states and service-error retries.
- Markdown and JSON conversation exports, including reasoning and attachment
  references, plus browser-session access to failed saves and a retry control.
- Serialized conversation writes and periodic partial-reply saves; navigation
  keeps late replies in their original conversation. Deletes wait for existing
  saves and prevent late stream finalizers from recreating a deleted chat.
- Strict stream completion, CRLF/multiline SSE support and visible errors for
  interrupted replies. Removed/uploading attachments no longer corrupt the
  pending list, and preview URLs are released after removal or sending.

Go fixtures exercise disk failures and local search. Chromium checks exercise
ordered writes, autosaves, navigation after failed saves, exports, interrupted
streams, search recovery and deletion failures. Live inference remains separate.

## Remaining validation and follow-up

1. Verify representative chat/image/voice inference on supported hardware with
   current upstream models and engine assets. Hugging Face access is currently
   blocked by this cloud environment's proxy; fixture checks do not establish
   live model compatibility. Recommendations have not been changed.
2. Add explicit architecture/recipe validation for arbitrary discovered models,
   beyond engine/platform availability. A popularity or recency score cannot
   establish local compatibility.
3. Consider a recommendation refresh policy once selected recipes can be
   validated against live upstream sources. Storage checks now cover individual
   transfers; aggregate reservations and engine-extraction estimates remain
   follow-up work.

Keep new work on Resolve/Pull/WithServer paths, with failure-path checks, and
preserve loopback authentication and artifact verification.
