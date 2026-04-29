# Bootstrap

`bootstrap` is the dedicated path for initial remote-to-remote seeding when the target does not yet contain the managed refs. It avoids decoding the fetched source objects into a local in-memory object store: the source pack is streamed directly into target `receive-pack`. See [architecture.md](architecture.md#memory-assumptions) for why that matters.

## Scope

`bootstrap` is intentionally narrow:

- create-only
- fails if any managed target ref already exists
- branch refs by default
- optional `--tags`
- optional explicit `--map`
- no `--force`
- no `--prune`
- no mixed create and update runs
- no automatic fallback to materialized `sync`
- smart HTTP only

## Flow

1. List source refs.
2. List target refs.
3. Build the managed ref set from `--branch`, `--map`, and `--tags`.
4. Fail if any managed target ref already exists.
5. Build create commands for the target.
6. Ask source for a pack containing the selected source tips.
7. Strip protocol framing and sideband (see [protocol.md](protocol.md#relay-framing)).
8. Stream the resulting pack directly into target `receive-pack`.
9. Parse target `report-status` and return a create summary.

## Failure Rules

`bootstrap` fails when:

- any managed target ref already exists
- no source refs matched
- the source fetch cannot be relayed cleanly
- target push fails

When a target is no longer in bootstrap shape, the error directs the operator to use `sync` instead.

## Auto-bootstrap inside `sync`

`sync` selects the bootstrap relay path automatically when all managed target refs are absent and the run matches bootstrap semantics. `plan` surfaces a bootstrap suggestion for the same target shape. Operators rarely need to invoke `bootstrap` directly — `sync` covers both initial seeding and ongoing sync with the same command.

## Batched bootstrap

For very large initial migrations where a single source pack is too big for comfortable target-side unpacking, `--target-max-pack-bytes` enables a batched mode that splits the bootstrap into multiple relay batches with temporary refs. It requires source-side protocol v2 with fetch filter support, and uses temporary target refs under `refs/gitsync/bootstrap/heads/`. See [bootstrap-batching.md](bootstrap-batching.md) for the algorithm and operator guidance.

## Related

- [bootstrap-batching.md](bootstrap-batching.md) — checkpoint batching for very large initial migrations
- [incremental-relay.md](incremental-relay.md) — narrow relay fast path for non-empty targets inside `sync`
- [protocol.md](protocol.md) — pkt-line, sideband, relay framing
- [architecture.md](architecture.md) — operation modes vs transfer modes, memory model
