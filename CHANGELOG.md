# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/),
and this project adheres to [Semantic Versioning](https://semver.org/).

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
- Documentation set: `docs/usage.md`, `docs/architecture.md`,
  `docs/protocol.md`, `docs/testing.md`, plus README installation,
  quick-start, and FAQ ([#21](https://github.com/entireio/git-sync/pull/21),
  [#22](https://github.com/entireio/git-sync/pull/22),
  [#23](https://github.com/entireio/git-sync/pull/23)).

### Changed

- Module path consolidated to `entire.io/entire/git-sync`, with the package
  moved to the repo root for ergonomic imports
  ([#5](https://github.com/entireio/git-sync/pull/5),
  [#12](https://github.com/entireio/git-sync/pull/12),
  [#15](https://github.com/entireio/git-sync/pull/15),
  [#17](https://github.com/entireio/git-sync/pull/17),
  [#27](https://github.com/entireio/git-sync/pull/27)).
- Go 1.26.2 minimum ([#16](https://github.com/entireio/git-sync/pull/16)).
- `go-git` upgraded to `v6.0.0-alpha.2`, which also pulls in the fix for
  CVE-2026-41506 ([#11](https://github.com/entireio/git-sync/pull/11)).
- Identical source and target endpoints are now rejected before any network
  round-trips.

### Fixed

- Worked around a `go-git` v6 upload-pack bug where the server emits two
  consecutive `NAK` pktlines in stateless-RPC mode when the client sends
  haves that are not reachable from any want. The second NAK was otherwise
  misread by the sideband demuxer as an unknown-channel frame.
- Hardened pkt-line parsing and error handling for malformed advertisements,
  oversized packets, and v2 remote `ERR` frames
  ([#14](https://github.com/entireio/git-sync/pull/14)).
- Bootstrap resume now finds the chain position from stale temp refs instead
  of failing on resume mismatch, and clears them when no recoverable
  position exists.
- Skip pack push for branches fully subsumed by trunk to avoid redundant
  receive-pack POSTs.

### Housekeeping

- GitHub Actions CI with `golangci-lint`, license check, and pinned action
  SHAs for supply-chain hygiene
  ([#7](https://github.com/entireio/git-sync/pull/7)).
- Integration-test harness against `git-http-backend`, plus end-to-end
  coverage of plan, dry-run, prune scope, identical-endpoint rejection, and
  incremental push failure recovery
  ([#18](https://github.com/entireio/git-sync/pull/18)).
- Removed Slack failure notification from the release workflow
  ([#28](https://github.com/entireio/git-sync/pull/28)).
