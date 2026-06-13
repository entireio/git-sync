# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/),
and this project adheres to [Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added

- Typed push-rejection errors on the public API. `Sync`/`Replicate` now report a `*RefRejectedError` (carrying the rejected `Ref` and the raw server `Reason`) for per-ref receive-pack `ng` statuses, reachable with `errors.As`. Rejections that are unambiguous concurrent target-ref moves — entire-server's compare-and-swap rejection (`remote ref has changed`) and git's `--force-with-lease` lease miss (`stale info`) — additionally satisfy `errors.Is(err, ErrTargetRefMoved)`. This lets embedders distinguish a benign racing concurrent push (retryable) from a genuine push failure without substring-matching the free-form error message. Ambiguous markers (`non-fast-forward` / `fetch first`) are deliberately excluded from the move classification so a real "needs `--force`" rejection is not masked. The `ForceWithLease` lease-failure escalation (raised even under `BestEffort`) also satisfies `errors.Is(err, ErrTargetRefMoved)`, though it is not itself a `*RefRejectedError` — prefer `errors.Is` over `errors.As` when you only need the cause. The error message and the underlying value-typed `packp.CommandStatusErr` are preserved unchanged (reach it with a value `errors.As` target), so existing checks keep working.

### Fixed

- Concurrent target-ref rejections are now actually classified — `errors.Is(err, ErrTargetRefMoved)` and `errors.As(err, *RefRejectedError)` match on the real push path. go-git returns `packp.CommandStatusErr` **by value** from `ReportStatus.Error()`, but `asRefRejectedError` / `annotateLeaseFailure` used a `*packp.CommandStatusErr` (pointer) `errors.As` target, which never matches a value in the chain — so every live receive-pack `ng` status passed through unclassified and `ErrTargetRefMoved` was never reported. Both now use a value target, and a regression test drives a real `ReportStatus.Error()` end to end so a pointer-vs-value relapse fails CI. (Bug in the unreleased typed-rejection feature above — never shipped in a tagged release.)

## [0.6.0] - 2026-06-03

### Added

- `git-sync convert-sha256`: one-off conversion that fetches a pack over smart HTTP from a SHA1 source, walks every reachable object via a two-pass topological DFS, and writes a fresh SHA256 bare repository with every tree/commit/tag reference re-encoded — including abbreviated SHA1 prefixes in commit messages. All branches and tags are always converted to avoid stranding cross-branch references; sharp edges and operational characteristics are documented in `docs/convert-sha256.md` ([#66](https://github.com/entireio/git-sync/pull/66))

### Changed

- HTTP auth now matches git's own flow: try anonymous first and only consult the credential helper after a 401, instead of proactively running `git credential fill` for every endpoint. This stops git-sync from dropping into an interactive `Username:`/`Password:` prompt on unauthenticated hosts and from leaking tokens to public repos. Expired credentials (401, or 403 from token services like Cloudflare) trigger a helper `reject` so the next run starts clean; the helper runs with `GIT_TERMINAL_PROMPT=0` to fail fast rather than block on a tty ([#65](https://github.com/entireio/git-sync/pull/65))
- Outbound requests now identify git-sync in the User-Agent instead of advertising only go-git's default. Git wire-protocol traffic (smart-HTTP info-refs, upload-pack/receive-pack, and the protocol v2 `agent=` capability) sends `git-sync/<version> go-git/<v>` to preserve go-git attribution; non-git HTTP (the GitHub repo metadata call during bootstrap) sends just `git-sync/<version>`. A new `internal/useragent` package centralises the format, wired from `versioninfo.Version` at startup so `--version` and the User-Agent agree, and overridable by SDK consumers ([#69](https://github.com/entireio/git-sync/pull/69))
- Bootstrap planning streams the commit-graph fetch instead of materializing the full commit set: a new `ExtractCommitParents` path parses the `tree:0` pack incrementally, extracting only `(commit -> parent hashes)` tuples with a bounded LRU for delta resolution. On `torvalds/linux` this cut peak Go heap from 5.42 GiB to 1.47 GiB (-73%), peak RSS from 5.69 GiB to 1.63 GiB (-71%), and wall time from 32m to 19m (-40%) ([#61](https://github.com/entireio/git-sync/pull/61))

### Fixed

- Materialized push against CDN-fronted HTTP targets (e.g. Cloudflare) no longer fails mid-upload with `use of closed network connection`. Two independent causes: a stale keep-alive pool entry expiring on the CDN edge during the gap between info-refs and receive-pack (fixed by disabling keep-alives on the default transport), and a mid-stream stall while go-git ran delta selection synchronously inside `Encode()` (fixed via `packfile.WithObjectSelector` to run selection up front and stream the write phase through `io.Pipe`). Adds `GITSYNC_HTTP_TRACE=1` for per-request lifecycle tracing with redacted auth, plus in-place verbose progress for the materialized encode/write phases ([#64](https://github.com/entireio/git-sync/pull/64))

### Housekeeping

- Bump go-git to v6.0.0-alpha.4 ([#60](https://github.com/entireio/git-sync/pull/60))

## [0.5.0] - 2026-05-18

### Added

- SSH transport: `ssh://`, SCP-style `git@host:path.git`, and `git+ssh://` remotes via the local `ssh` binary, with one process per logical RPC so v2 and batched flows work correctly. SSH config-driven user/key behavior is honored, and a clear error is raised when `ssh` is not on `PATH` ([#54](https://github.com/entireio/git-sync/pull/54), [#56](https://github.com/entireio/git-sync/pull/56))
- `--all-refs` for mirroring arbitrary `refs/*` namespaces (notes, pulls, custom) beyond `refs/heads/*` and `refs/tags/*`. For `sync` and `bootstrap` it bundles a best-effort failure mode that downgrades per-ref `receive-pack` rejections to warnings (surfaced via `Result.Warned` and JSON `warned`), so mirroring into hosts with hidden refs like GitHub `refs/pull/*` works. `replicate` keeps strict semantics. Library exposes `RefScope.AllRefs`, `SyncPolicy.BestEffort`, `RefKindOther`, and `ActionWarn` ([#44](https://github.com/entireio/git-sync/pull/44))
- Bootstrap pushes the source `HEAD`'s branch first, so hosts that pick the default branch from the first push on a fresh repo (GitHub, GitLab) end up with the right default automatically. The source `HEAD` symref is also surfaced on `Result` and `ProbeResult` (`execution.sourceHead` / `sourceHead` in JSON) ([#51](https://github.com/entireio/git-sync/pull/51))

### Changed

- `--force` is replaced by two explicit flags: `--force-with-lease` (previous lease-protected behavior) and `--force-blind` (zero expected-old, overwrite regardless — matches `git push --force`). The flags are mutually exclusive; legacy `--force` errors out with a migration hint. `bootstrap` and `replicate` continue to reject force flags entirely. `SyncPolicy.Force` splits into `ForceWithLease` and `ForceBlind`. Lease-failure `ng` responses from `receive-pack` are annotated with a rerun-or-`--force-blind` hint ([#53](https://github.com/entireio/git-sync/pull/53))
- `replicate` failure messages no longer suggest "use sync instead" for errors that aren't relay-capability problems (network, cancellation, etc.) ([#52](https://github.com/entireio/git-sync/pull/52))

### Fixed

- v1 target pushes against repos with annotated tags no longer fail with `HTTP 400 invalid reference name: refs/tags/<X>^{}`. `AdvRefsToSlice` now drops peeled `^{}` entries that go-git v6 alpha.3 preserves inline; affected `replicate` always and `sync --prune` ([#57](https://github.com/entireio/git-sync/pull/57))

### Housekeeping

- Bump go-git to v6.0.0-alpha.3 ([#49](https://github.com/entireio/git-sync/pull/49), [#55](https://github.com/entireio/git-sync/pull/55))

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
