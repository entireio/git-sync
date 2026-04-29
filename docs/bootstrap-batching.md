# Bootstrap Batching

`bootstrap` normally streams one source pack into one target push. That covers many initial syncs, but it is not enough for very large single-branch repositories where one initial pack is itself too large for comfortable target-side unpacking and indexing. Batched bootstrap splits the initial migration into multiple bounded relay batches with temporary refs, while preserving the main benefit of bootstrap: no full local object materialization, direct source-to-target relay, and clear restart points.

## Non-Goals

Batching does not try to solve every large-migration problem.

Out of scope:

- non-empty target incremental relay batching
- prune/delete behavior
- tag retargeting
- fully optimal pack-size planning
- arbitrary graph partitioning

## Why Per-Ref Batching Is Not Enough

Per-ref batching is easy:

- push `refs/heads/main`
- then `refs/heads/release`
- then tags

That helps when there are many refs and each ref is moderate in size.

It does not help for repositories where a single branch is enormous. Linux `master` is the motivating example: even a single branch bootstrap can be too large for one target-side unpack/index step.

## Model

Branch checkpoint batching with temporary refs:

1. Choose a sequence of ancestor checkpoints for a source branch.
2. Push them oldest to newest into a temporary target ref.
3. Once the final tip is present, create the real target ref.
4. Delete the temporary ref.

This gives bounded per-push transfer size, bounded target-side unpack/index work per batch, restart points between batches, and no partially initialized real branch refs visible unless the run finishes.

The CLI surface is `git-sync bootstrap --target-max-pack-bytes <bytes>`. (The same flag is also exposed on `sync`, which auto-bootstraps on empty targets.) Batched bootstrap requires source-side protocol v2 with fetch filter support.

## Temporary Ref Strategy

For each target branch:

- real target ref: `refs/heads/main`
- temp bootstrap ref: `refs/gitsync/bootstrap/heads/main`

Flow:

1. Create/update only the temp ref during intermediate batches.
2. After the final tip batch succeeds:
   - create the real ref at the final tip
   - delete the temp ref

This keeps the target repository in a cleaner state:

- before completion, the real branch is absent
- after completion, only the final branch remains

If failure happens mid-run:

- the temp ref records progress
- the real ref is still absent

## Checkpoint Selection

Checkpoints are placed using a commit-count estimate, not measured pack sizes.

For a branch tip:

1. Fetch the commit graph (tree:0 filter, one round-trip — commits only, no blobs/trees).
2. Walk first-parent ancestry backward to get the chain length.
3. Estimate total pack size: `chainLen × 8 KiB/commit`.
4. Compute number of batches: `ceil(estimated / --target-max-pack-bytes)`.
5. Place checkpoints evenly along the first-parent chain.

This is a heuristic — real bytes-per-commit varies widely (2–100+ KiB depending on blob churn). The estimate intentionally errs toward more batches.

### Adaptive size correction

If the estimate is too optimistic (fewer batches than needed), two safeguards catch it:

1. **PACK header pre-check**: after starting a fetch, peek at the first 12 bytes of the pack to read the object count. Multiply by ~750 bytes/object. If the estimate exceeds `--target-max-pack-bytes`, abort the fetch (12 bytes wasted, not gigabytes), insert a midpoint checkpoint, and retry. This avoids a full transfer for obviously-oversized batches.

2. **Target rejection retry**: if the target's receive-pack rejects a push for exceeding its body-size limit, detect the error, insert a midpoint checkpoint from the stored chain, and retry. This catches cases where the PACK header estimate was close but the real pack was slightly over.

Both safeguards converge in O(log n) splits — each failure halves the commit range.

### Trunk-aware planning

For multi-branch bootstraps, planning each branch in isolation re-fetches commit graph history that earlier branches have already reached. With one trunk and many feature branches that all descend from it, that becomes N full-history fetches and N independent first-parent walks over the same shared commits.

`git-sync` avoids this by:

1. Identifying the trunk via the source's HEAD symref (see [protocol.md](protocol.md#head-symref-discovery)) and ordering it first.
2. While planning the trunk, recording its first-parent commit set as `trunkStopSet` and its tip in `trunkHaves`.
3. For each subsequent branch, passing `trunkHaves` as `have` lines on the commit-graph fetch, and stopping the first-parent walk when it hits a commit in `trunkStopSet`.
4. Skipping the pack push entirely when a branch tip is already in `trunkStopSet` — a *subsumed* branch. The only command emitted is a single ref-create to point the target ref at the tip, optionally combined with a temp-ref delete if a previous interrupted run left one behind.

The state is seeded once from the trunk and reused across all later branches; it is not re-accumulated after each branch. Falls back to the per-branch behavior described above when HEAD is not advertised, or when the trunk ref is filtered out by `--branch` / `--map`.

### Why not probe (the previous design)

The previous implementation did full `FetchPack` round-trips per probe candidate to measure actual pack sizes. For linux/master (75k commits) this required 13+ fetch-and-discard cycles, downloading gigabytes of throwaway data and taking minutes before any real push started. The estimate approach reduces planning to one commit-graph fetch (~20 seconds) plus arithmetic.

## Batch Flow For One Branch

Given checkpoints:

- `C1`
- `C2`
- `C3`
- `tip`

The flow is:

1. Fetch source pack for `C1` with no `have`.
2. Push it to temp ref `refs/gitsync/bootstrap/heads/main`.
3. Fetch source pack for `C2` with `have=C1`.
4. Push update of temp ref to `C2`.
5. Fetch source pack for `C3` with `have=C2`.
6. Push update of temp ref to `C3`.
7. Fetch source pack for `tip` with `have=C3`.
8. Push update of temp ref to `tip`.
9. Create real target ref `refs/heads/main` at `tip`.
10. Delete temp ref.

The final ref creation is a separate tiny push with no pack — easier to reason about than combining it with the last batch.

## Tags

Branch batches push first; create-only tags are pushed after all branch batches complete, so a tag never points at an object graph that is not yet fully present on the target. Tag retargeting is not supported in the batched path.

## Restart and Recovery

On rerun, batched bootstrap detects existing temp refs on the target, resolves their current hashes, and resumes from the latest completed checkpoint instead of starting from zero.

## Safety Model

Batching remains strict. It allows only:

- empty managed target refs
- branch-only bootstrap
- no force
- no prune
- no existing real target refs for the managed branches

Fail if:

- temp refs already exist but do not match expected checkpoint progression
- estimated batch sizing cannot find a checkpoint under the configured limit
- final ref cutover fails

## Operator Output

Batching surfaces explicitly in both text and JSON output: `batching`, `batchCount`, `plannedBatchCount`, and `tempRefs` appear as top-level keys in `--json`, with the same values mirrored in the text summary. Verbose mode (`-v`) prints temp ref names and per-batch checkpoint progress.

## Practical Risks

- the 8 KiB/commit estimate can be significantly off for blob-heavy repos (linux is ~66 KiB/commit); the PACK header pre-check and target-rejection retry handle this adaptively
- object density is not uniform along the commit chain — recent history often has more objects per commit than early history, so evenly-spaced checkpoints produce uneven pack sizes
- target-side unpack/index cost may still be high even after batching, just smaller
- temp refs add cleanup and restart complexity
- the source builds the full pack even if we abort after the PACK header; this wastes source CPU but not network

This is still likely worthwhile for very large initial migrations because it changes a single huge risky operation into several bounded ones.

## Operator Guidance

- Prefer plain `bootstrap` (or `sync` against an empty target) first.
- Use batching when a single large bootstrap push is too risky, too large, or fails on the target side.
- Start with `--target-max-pack-bytes 536870912` and adjust upward only if the target has enough headroom.

Batched bootstrap is exercised by `TestBootstrap_GitHTTPBackendBatchedBranch` and validated against `torvalds/linux` as a large-source manual stress path. The implementation does not parallelize across branches and does not extend the same checkpoint-batching idea to non-empty target incremental relay.
