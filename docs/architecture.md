# Architecture

`git-sync` is a remote-to-remote Git mirroring CLI over smart HTTP.

## Product Rationale

The point of `git-sync` is not that Git mirroring is impossible without it. The point is that the usual alternatives are awkward at the exact layer operators often need:

- a full local mirror clone and mirror push is simple, but it turns remote-to-remote movement into a local storage and local bandwidth problem
- host-specific migration tools are useful, but they are not portable and they usually do not expose one consistent sync primitive across providers
- scripts around `git fetch` and `git push` can work, but they usually lack planning, explicit policy checks, stable machine-readable output, and a clean distinction between bootstrap and incremental sync

`git-sync` is meant to be that missing middle layer:

- provider-agnostic
- remote-to-remote
- automation-friendly
- explicit about safety and relay eligibility
- capable of handling both first-time seeding and repeat syncs

That is why the design leans so heavily on:

- relay-first strategies
- front-loaded validation
- typed results and JSON output
- explicit execution modes instead of a single opaque "mirror" operation

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
  The fallback remains intentionally bounded: non-relay object materialization is kept in memory and guarded by an explicit object-count limit rather than being treated as unbounded.
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

## Memory Assumptions

The relay paths and the materialized fallback have very different memory stories.

- Relay paths scale with streaming behavior.
  The source computes the pack, `git-sync` coordinates the transfer, and the target receives it directly. Large repositories are expected to stay viable primarily through bootstrap and incremental relay.
- Materialized fallback scales with the local object set that must be pushed.
  Once `git-sync` stops relaying and starts building a local push, it must hold the relevant Git objects in memory long enough to compute object closure and encode the outgoing pack.

Useful rules of thumb:

- Small branch delta fallback:
  Target already has the old branch tip, source has a few new commits, and the repo is mostly text/code.
  Memory is driven by the new commits, trees, and blobs above the target tip, not the full repo history.
  This is the most reasonable non-relay case.

- Broad fallback without shared history:
  Relay is unavailable and the target is missing most of the history or object graph behind the refs being updated.
  Memory can approach a large fraction of the pushed object set, especially if the repo contains large blobs.
  This is the risky case for the in-memory fallback.

- Ref-only delete or tiny tag case:
  Delete-only operations are effectively ref-only and do not need an object closure.
  Lightweight tag creation can also be close to ref-only when the target already has the underlying commit/tree/blob objects.
  These are cheap even without relay.

The important distinction is that "repo size" alone is not a sufficient predictor. For materialized fallback, the practical questions are:

- how many objects need to be sent
- how large the missing blobs are
- how much object overlap already exists on the target

That is why the rewrite keeps an explicit `--materialized-max-objects` guardrail. It is not a precise heap model; it is a coarse safety rail for the in-memory fallback path.

## Related Notes

- [bootstrap.md](/Users/soph/Work/entire/devenv/git-sync/docs/bootstrap.md)
- [bootstrap-batching.md](/Users/soph/Work/entire/devenv/git-sync/docs/bootstrap-batching.md)
- [benchmarking.md](/Users/soph/Work/entire/devenv/git-sync/docs/benchmarking.md)
- [rewrite-memo.md](/Users/soph/Work/entire/devenv/git-sync/docs/rewrite-memo.md)
