# Incremental Relay

`sync` has a narrow relay fast path for safe incremental updates. When eligible, it streams a fetched source pack directly into target `receive-pack` instead of decoding the object graph into the local in-memory store and re-encoding a push pack. This keeps the in-memory cost near zero for the common case where a sync run only needs to forward a small amount of new history.

This document describes when the fast path applies, what it covers, and when `sync` falls back to the materialized path.

## Eligibility

The incremental relay path is selected only when **all** of the following hold:

- no `--force`
- no `--prune`
- no managed-ref deletes
- no tag retargeting (creating a new tag at a new tip is allowed; moving an existing tag is not)
- target advertises `no-thin` on `receive-pack`

`no-thin` is the load-bearing capability. The relayed pack must be self-contained because git-sync's source fetch never requests the `thin-pack` capability, so the target can apply the pack directly without resolving deltas against existing target objects. See [protocol.md](protocol.md) for the framing details.

## What the fast path covers

When the eligibility conditions hold, the relay path covers:

- multi-branch fast-forward branch updates
- branch-to-branch ref mappings (`--map src:dst`)
- create-only tag pushes that fit alongside the branch updates

In other words: the everyday "mirror these branches and any new tags forward" case is fully covered.

## What still falls back to materialized

The materialized path (decode source objects into the local store, plan the push set, encode a target pack) still handles:

- `--force` and any non-fast-forward update
- `--prune` and managed-ref deletes
- tag retargeting (an existing tag pointing at a new object)
- runs against targets that don't advertise `no-thin`

The materialized path is bounded by `--materialized-max-objects` as a safety guardrail. See [architecture.md](architecture.md#memory-assumptions) for the memory model.

## Relationship with `bootstrap`

`sync` also auto-selects the bootstrap relay path when all managed target refs are absent and the run otherwise matches bootstrap semantics. That is a separate code path (in `internal/strategy/bootstrap`) and is documented in [bootstrap.md](bootstrap.md). The incremental relay path discussed here is for non-empty targets where the existing tips can be advertised as `have` lines during source fetch.

The decision flow inside `sync` is:

1. If target has none of the managed refs and the run is bootstrap-compatible → bootstrap relay path
2. Else if all eligibility conditions for incremental relay hold → incremental relay path
3. Else → materialized fallback (bounded by `--materialized-max-objects`)

## Why this matters

For repeat sync jobs against an actively used mirror, the common case is "a few new commits on a couple of branches plus maybe a new tag." Without the incremental relay path, every such run would decode the fetched objects into the in-memory store, then re-encode them into a push pack. With relay, the runner forwards a self-contained source pack directly, and the per-run memory and CPU cost stays roughly proportional to the size of the new history rather than the size of the touched repos.

## Implementation

The incremental relay strategy lives in `internal/strategy/incremental`. The shared relay framing, sideband stripping, and PACK header handling live in `internal/gitproto`. See [protocol.md](protocol.md) for protocol-level details and [architecture.md](architecture.md) for where this fits in the overall package layout.

## Related

- [bootstrap.md](bootstrap.md) — empty-target relay (auto-selected by `sync` when applicable)
- [bootstrap-batching.md](bootstrap-batching.md) — large initial migration via temp-ref batching
- [replicate.md](replicate.md) — source-authoritative relay-only overwrite mode
- [architecture.md](architecture.md) — operation modes vs transfer modes
