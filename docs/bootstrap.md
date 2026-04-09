# Bootstrap Design

`bootstrap` is a planned command path for initial remote-to-remote seeding when the target does not yet contain the managed refs.

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

## V1 Scope

`bootstrap` should be intentionally narrow:

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

V1 should stay strict rather than trying to be clever.

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

V1 should fail when:

- any managed target ref already exists
- no source refs matched
- the source fetch cannot be relayed cleanly
- target push fails

The error should explicitly recommend normal `sync` when the repository is no longer in bootstrap shape.

## Follow-Up Steps

Phase 1:

- implement `bootstrap` for create-only branch refs
- support optional tag creation
- add JSON and stats output
- add in-process integration tests
- add `git-http-backend` integration coverage for empty-target bootstrap

Phase 2:

- allow relay-safe create-only runs with explicit mapped refs
- add better operator output for large initial transfers
- add safety thresholds for advertised/fetched bytes

Progress:

- explicit mapped refs are supported
- `--max-pack-bytes` provides a first safety threshold for the streamed source pack during bootstrap

Phase 3:

- investigate hybrid behavior: relay when the target is empty, otherwise fail fast into normal `sync`
- investigate whether target capability combinations require alternate pack handling
- measure source-to-target pack relay memory and CPU against current `sync`

Progress:

- `sync` now auto-selects the bootstrap relay path when all managed target refs are absent and the run matches bootstrap semantics
- dry-run `plan` surfaces a bootstrap suggestion for the same target shape

Phase 4:

- consider a more advanced incremental relay mode for non-empty targets
- only pursue this if large migration workflows become important enough to justify the added protocol complexity

Progress:

- there is now a narrow incremental relay path in `sync`
- it now covers multi-branch fast-forward branch-only updates
- tags, deletes, force, prune, and mapped refs still use the normal path
