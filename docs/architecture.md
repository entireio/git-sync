# Architecture

`git-sync` is a remote-to-remote Git mirroring CLI over smart HTTP.

## Core Decisions

### Relay First

The main product decision is to prefer pack relay over local decode/re-encode when that is safe:

- fetch a pack from source
- avoid materializing the full object graph locally when possible
- stream into target `receive-pack`

That is why bootstrap and incremental relay are explicit strategies instead of hidden optimizations.

### Explicit Strategy Split

The current execution modes are:

- `bootstrap`
  - empty-target relay
- `sync`
  - planning plus reconciliation
- incremental relay
  - narrow fast path for safe updates
- materialized fallback
  - decode/repack path when relay is not safe
- batched bootstrap
  - large initial migration fallback

## Package Model

- `internal/gitproto`
  - smart HTTP, pkt-line, fetch/push request handling, capability negotiation
- `internal/planner`
  - desired refs, prune policy, action planning, checkpoint planning
- `internal/validation`
  - input normalization and front-loaded validation
- `internal/auth`
  - credential lookup, Entire token handling, token store behavior
- `internal/strategy/bootstrap`
  - one-shot relay bootstrap and batched bootstrap
- `internal/strategy/incremental`
  - narrow incremental relay path
- `internal/strategy/materialized`
  - local object materialization and encode/repack push
- `internal/syncer`
  - top-level orchestration and result shaping
- `internal/syncertest`
  - shared in-memory test fixtures

## Protocol Boundaries

- Source discovery and source fetch can use protocol v2 when supported.
- Push remains on the current `receive-pack` path.
- `--protocol auto` prefers source-side v2 and falls back to v1.
- `--protocol v2` requires the source remote to negotiate v2.

Protocol v2 is used where it materially improves discovery and fetch behavior. Push stays on the existing low-level path because the tool already needs explicit command construction and streaming control there.

## Current Constraints

- smart HTTP only
- no local working tree
- explicit ref mapping, not wildcard mirroring
- objects still remain in memory for the duration of materialized paths
- batched bootstrap is intentionally narrower than normal sync

## Related Notes

- [bootstrap.md](/Users/soph/Work/entire/devenv/git-sync/docs/bootstrap.md)
- [bootstrap-batching.md](/Users/soph/Work/entire/devenv/git-sync/docs/bootstrap-batching.md)
- [benchmarking.md](/Users/soph/Work/entire/devenv/git-sync/docs/benchmarking.md)
- [rewrite-memo.md](/Users/soph/Work/entire/devenv/git-sync/docs/rewrite-memo.md)
