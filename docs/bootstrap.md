# Bootstrap Design

`bootstrap` is the dedicated command path for initial remote-to-remote seeding when the target does not yet contain the managed refs.

The goal is to avoid decoding the fetched source objects into the local in-memory object store during an initial sync. Instead, `bootstrap` should fetch a pack from the source and relay it directly into target `receive-pack`.

## Why

The current `sync` path is optimized for general incremental reconciliation:

- it fetches from source with target tip hashes as `have`
- it builds plans locally
- it stores fetched source objects in a local object store
- it computes the object closure to push
- it encodes a new pack for target

That is a good general path, but it is a poor fit for very large initial syncs into an empty target because the missing object graph must fit in local memory.

`bootstrap` is meant to cover the opposite case:

- target refs are absent
- all actions are creates
- there is no need for fast-forward checks
- the main cost is moving a large pack from source to target efficiently

## Scope

`bootstrap` is intentionally narrow:

- create-only
- fail if any managed target ref already exists
- branch refs by default
- optional `--tags`
- optional explicit `--map`
- no `--force`
- no `--prune`
- no mixed create and update runs
- no automatic fallback to normal `sync`
- smart HTTP only

This command is for first-time seeding. After that, operators should use `sync`.

## Command Shape

Preferred CLI:

```bash
git-sync bootstrap [flags] <source-url> <target-url>
```

Expected v1 flags:

- `--branch`
- `--map`
- `--tags`
- `--max-pack-bytes`
- `--stats`
- `--json`
- `--protocol auto|v1|v2`
- existing source and target auth flags

## Intended Flow

1. List source refs.
2. List target refs.
3. Build the managed ref set from `--branch`, `--map`, and `--tags`.
4. Fail if any managed target ref already exists.
5. Build create commands for the target.
6. Ask source for a pack containing the selected source tips.
7. Strip protocol framing and sideband as needed.
8. Stream the resulting pack directly into target `receive-pack`.
9. Parse target report-status and return a create summary.

## Why This Helps

The large memory cost in the current implementation comes from storing fetched source objects locally before re-encoding them.

`bootstrap` should avoid that cost for initial syncs by not materializing the object graph in local storage unless a fallback path is explicitly chosen later.

The expected wins are:

- much lower RAM usage for empty-target syncs
- less local CPU spent decoding and re-encoding large object graphs
- better fit for large repo migrations

## Constraints

There are still some hard limits:

- source and target still need normal smart HTTP discovery
- target policy can still reject pushes
- push still depends on target `receive-pack` behavior and capabilities
- if a relay-safe path cannot be used, `bootstrap` should fail and tell the user to use `sync`

`bootstrap` stays strict rather than trying to be clever.

## Implementation Notes

The cleanest implementation shape is a separate code path, not an optimization hidden inside `sync`.

Suggested pieces:

- `runBootstrap` in `cmd/git-sync/main.go`
- `syncer.Bootstrap(ctx, cfg)` in `internal/syncer`
- source fetch helper that returns a pack stream instead of writing objects into storage
- target receive-pack helper that accepts an externally supplied pack stream
- bootstrap-specific result type or reuse `Result` with only create actions

The initial implementation should prefer:

- one multi-ref source fetch
- one multi-command target push

That keeps it efficient and conceptually simple.

## Failure Rules

`bootstrap` fails when:

- any managed target ref already exists
- no source refs matched
- the source fetch cannot be relayed cleanly
- target push fails

The error should explicitly recommend normal `sync` when the repository is no longer in bootstrap shape.

## Current Behavior

Bootstrap supports:

- create-only branch refs
- optional tag creation (non-batched path and after successful branch batches in the batched path)
- explicit mapped refs (`--map src:dst`)
- JSON and `--stats` output
- `--max-pack-bytes` as a safety threshold for the streamed source pack
- `--target-max-pack-bytes` for batched branch-only bootstrap on very large initial syncs
- in-process integration coverage and `git-http-backend` integration coverage

`sync` auto-selects the bootstrap relay path when all managed target refs are absent and the run matches bootstrap semantics. `plan` surfaces a bootstrap suggestion for the same target shape.

The batched bootstrap path is intentionally narrow:

- requires source-side protocol v2 with fetch filters
- batches branch refs, then optionally creates tags after the branch batches complete
- resumes from an existing temp ref when that temp ref matches a planned checkpoint
- uses temporary target refs under `refs/gitsync/bootstrap/heads/`
- treat as an advanced large-repo fallback when one-shot bootstrap is too risky or fails under target-side unpack/index pressure

Outside bootstrap, `sync` also has a narrow incremental relay path for non-empty targets that covers multi-branch fast-forward updates, branch-to-branch mappings, and create-only tags. Tag retargets, deletes, force, and prune still use the materialized path. See [incremental-relay.md](incremental-relay.md).
