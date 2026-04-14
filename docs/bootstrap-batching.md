# Bootstrap Batching Design

`bootstrap` currently streams one source pack into one target push. That is good for many initial syncs, but it is not enough for very large single-branch repositories where one initial pack is itself too large for comfortable target-side unpacking and indexing.

This note sketches a batching design for large bootstrap jobs.

## Goal

Reduce per-push size and target-side `receive-pack` / `index-pack` pressure for very large initial syncs, while preserving the main benefit of bootstrap:

- no full local object materialization in `git-sync`
- direct source-to-target relay
- clear operator-visible progress and restart points

## Non-Goals

V1 batching should not try to solve every large-migration problem.

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

## Preferred Model

Use branch checkpoint batching with temporary refs.

High-level idea:

1. Choose a sequence of ancestor checkpoints for a source branch.
2. Push them oldest to newest into a temporary target ref.
3. Once the final tip is present, create the real target ref.
4. Delete the temporary ref at the end.

This gives:

- bounded per-push transfer size
- bounded target-side unpack/index work per batch
- restart points between batches
- no partially initialized real branch refs visible unless the run finishes

## Command Shape

Possible CLI extension:

```bash
git-sync bootstrap \
  --target-max-pack-bytes 1073741824 \
  <source-url> \
  <target-url>
```

Possible related flags:

- `--target-max-pack-bytes`
- `--batch-ref-prefix refs/gitsync/bootstrap/`
- `--keep-temp-refs-on-failure`

The first version should only need `--target-max-pack-bytes`.

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

The final ref creation can be:

- one separate tiny push with no pack
- or combined with the last batch if command ordering and push semantics stay clear

The first version should prefer the separate final ref creation because it is easier to reason about.

## Tags

Tags should not be pushed during intermediate branch batches.

Recommended rule:

- branch batches first
- tag creation only after all referenced branch/object checkpoints complete

This avoids cases where a tag points at an object graph that is not yet fully present on target.

V1 batching should support:

- branch refs only

Tag batching can be added later.

## Restart and Recovery

Batching is only worth doing if failures are restartable.

Minimum restart model:

1. Detect existing temp refs on target.
2. Resolve their current hashes.
3. Resume from the latest completed checkpoint instead of starting from zero.

If `--keep-temp-refs-on-failure` is false, cleanup can still happen on clean failures, but default restartability is more valuable than aggressive cleanup.

## Safety Model

V1 batching should remain strict.

Allow only:

- empty managed target refs
- branch-only bootstrap
- no force
- no prune
- no existing real target refs for the managed branches

Fail if:

- temp refs already exist but do not match expected checkpoint progression
- estimated batch sizing cannot find a checkpoint under the configured limit
- final ref cutover fails

## Implementation Shape

Suggested pieces:

- `BootstrapBatch` execution path in `internal/syncer`
- checkpoint planner:
  - first-parent ancestry walker
  - batch-size estimator
- temp ref naming helpers
- resume detector for existing temp refs
- final cutover helper

The estimator should reuse the existing relay mechanics:

- source fetch with `have`
- streamed push to target

But it will need one new planning pass to probe likely batch sizes before actual execution.

## Operator Output

Batching should be explicit in output.

Text output should include:

- `batching=true`
- per-branch checkpoint count
- current batch number
- temp ref names when verbose

JSON should include:

- `batching`
- `batch_count`
- `completed_batches`
- `temp_refs`

## Practical Risks

- the 8 KiB/commit estimate can be significantly off for blob-heavy repos (linux is ~66 KiB/commit); the PACK header pre-check and target-rejection retry handle this adaptively
- object density is not uniform along the commit chain — recent history often has more objects per commit than early history, so evenly-spaced checkpoints produce uneven pack sizes
- target-side unpack/index cost may still be high even after batching, just smaller
- temp refs add cleanup and restart complexity
- the source builds the full pack even if we abort after the PACK header; this wastes source CPU but not network

This is still likely worthwhile for very large initial migrations because it changes a single huge risky operation into several bounded ones.

## Recommended Phases

Phase A:

- batch branch-only bootstrap
- no tags
- temp refs required
- no resume
- manual cleanup if interrupted

Progress:

- implemented via `git-sync bootstrap --target-max-pack-bytes`
- currently requires source-side protocol v2 with fetch filter support
- resumes from an existing temp ref when that temp ref matches a planned checkpoint
- exercised by `TestBootstrap_GitHTTPBackendBatchedBranch`
- validated against `torvalds/linux` as a large-source manual stress path

Operator guidance:

- prefer plain `bootstrap` first
- use batching when a single large bootstrap push is too risky, too large, or fails on the target side
- start with `--target-max-pack-bytes 536870912` and adjust upward only if the target has enough headroom

Phase B:

- add resume from existing temp refs
- add better progress reporting
- add batch-size estimation metrics

Phase C:

- consider tag creation after successful branch completion
- consider whether per-ref or per-branch parallelism is worth it

Progress:

- create-only tags are now pushed after successful branch batches complete

Phase D:

- only then consider using similar checkpoint batching ideas for non-empty target incremental relay
