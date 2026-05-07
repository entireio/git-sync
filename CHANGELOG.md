# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/),
and this project adheres to [Semantic Versioning](https://semver.org/).

## [0.4.3] - 2026-05-07

### Added

- `--bootstrap-strategy=topo` for merge-heavy repos: walks every reachable commit in deterministic topological order so batched bootstrap can place sub-pack boundaries inside side-branch ancestry ([#41](https://github.com/entireio/git-sync/pull/41))
- `--progress` for live per-side throughput across `sync`, `replicate`, `bootstrap`, and `fetch`, with rolling-window rate, hostname-aware labels, inline pack subdivision, and an end-of-run summary under `--stats` ([#37](https://github.com/entireio/git-sync/pull/37))
- Per-subcommand `--help` after the CLI moved to cobra; bare `git-sync` lists subcommands on stdout instead of printing the full usage block as an error ([#35](https://github.com/entireio/git-sync/pull/35))
- README cover image and embedded demo videos for `git-sync plan` and `git-sync sync` ([#30](https://github.com/entireio/git-sync/pull/30), [#31](https://github.com/entireio/git-sync/pull/31))

### Changed

- Batched bootstrap stream-parses the pack and aborts doomed pushes ~5% in instead of waiting for the body-limit rejection; subdivision converges in 1–2 rounds instead of 6+ on blob-heavy repos ([#40](https://github.com/entireio/git-sync/pull/40))
- Post-rejection subdivision sizes splits from observed pack bytes (4× when the server cut us off mid-stream, 2× otherwise) and ratchets the bytes-per-object estimate up after each 413 ([#38](https://github.com/entireio/git-sync/pull/38))
- Batched bootstrap recombines upcoming checkpoints when consecutive packs underuse the target limit, recovering pack granularity after heavy regions; already-pushed checkpoints are also passed as fetch `have`s on later rounds ([#42](https://github.com/entireio/git-sync/pull/42))
- Relay-only syncs skip the upfront `FetchToStore`; the materialized fallback lazy-fetches the source closure only when force, prune, or divergent refs require it ([#34](https://github.com/entireio/git-sync/pull/34))

### Fixed

- Sync into targets that share reachability with the source: tolerate pruned objects in the materialized walker, accept branch creates and `no-thin` targets in incremental relay, and surface diagnostic headers (`Cf-Ray`, `Server`, `X-Request-Id`) on opaque 5xx ([#33](https://github.com/entireio/git-sync/pull/33))

## [0.4.2] - 2026-04-30

First public release. `git-sync` mirrors refs from a source remote to a target
remote without a local checkout, streaming source packs directly into target
`receive-pack` whenever possible. The release covers the CLI, the library API,
and the protocol plumbing they share.

### Added

- `git-sync sync` — relay-based mirror that streams source `upload-pack`
  output into target `receive-pack` without materializing the object graph
  locally. Falls back to an in-memory `go-git` store, bounded by
  `--materialized-max-objects`, when relay is not eligible (force, prune,
  deletes, tag retargets) ([#1](https://github.com/entireio/git-sync/pull/1),
  [#2](https://github.com/entireio/git-sync/pull/2)).
- `git-sync replicate` and `git-sync plan --mode replicate` for
  source-authoritative, relay-only replication. Divergent branches and tags
  are retargeted against the source; `--prune` deletes orphan managed refs.
  Relay-only by design: no materialized fallback
  ([#4](https://github.com/entireio/git-sync/pull/4)).
- `git-sync plan` — preview the actions a `sync` or `replicate` would take,
  with structured JSON output suitable for automation.
- `git-sync bootstrap` — initial-seed path for empty targets, with adaptive
  batching, trunk-first planning to cut per-branch graph fetches, and
  resume-from-stale-temp-refs recovery
  ([#6](https://github.com/entireio/git-sync/pull/6)).
- `git-sync version` subcommand with build metadata
  ([#26](https://github.com/entireio/git-sync/pull/26)).
- Reusable Go library at `entire.io/entire/git-sync`. The stable surface
  (`Probe`, `Plan`, `Sync`, `Replicate`, typed results, auth and HTTP
  injection) lives at the module root; advanced controls (`Bootstrap`,
  `Fetch`, batching knobs, heap measurement) live in
  `entire.io/entire/git-sync/unstable`
  ([#3](https://github.com/entireio/git-sync/pull/3),
  [#17](https://github.com/entireio/git-sync/pull/17)).
- Git protocol v2 source-side support: `ls-refs`, `fetch` with v2
  acknowledgments and response-end handling, capability negotiation, and
  graceful fallback when the source does not advertise v2.
- Smart HTTP transport: pkt-line primitives, sideband demultiplexing, info/refs
  advertisement validation, smart endpoint path normalization, oversized
  packet rejection, empty pkt-line acceptance, and v2 fetch remote `ERR`
  packet handling.
- Optional info/refs redirect following on the source endpoint, exposed
  through the public `gitsync` API
  ([#9](https://github.com/entireio/git-sync/pull/9)).
- Git credential helper fallback and `--source-token` / `--target-token`
  flags for HTTPS auth.
- JSON output mode with a stable schema and camelCase keys across all
  commands ([#7](https://github.com/entireio/git-sync/pull/7)).
- Adaptive bootstrap batching: auto-subdivide on target body-size rejection,
  pre-check PACK header object count before pushing oversized batches, and
  shared `--max-pack-bytes` / `--target-max-pack-bytes` flags across `sync`,
  `replicate`, `plan`, and `bootstrap`.
- Sideband progress streamed to stderr when `-v` is set.
- Homebrew tap install via `brew tap entireio/tap && brew install --cask git-sync`
  ([#25](https://github.com/entireio/git-sync/pull/25)).
- GoReleaser-based release pipeline for cross-platform binaries
  ([#26](https://github.com/entireio/git-sync/pull/26)).
- Identical source and target endpoints are rejected before any network
  round-trips.
- Documentation set: `docs/usage.md`, `docs/architecture.md`,
  `docs/protocol.md`, `docs/testing.md`, plus README installation,
  quick-start, and FAQ ([#21](https://github.com/entireio/git-sync/pull/21),
  [#22](https://github.com/entireio/git-sync/pull/22),
  [#23](https://github.com/entireio/git-sync/pull/23)).
