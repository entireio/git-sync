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
  --batch-max-pack-bytes 1073741824 \
  <source-url> \
  <target-url>
```

Possible related flags:

- `--batch-max-pack-bytes`
- `--batch-ref-prefix refs/gitsync/bootstrap/`
- `--keep-temp-refs-on-failure`

The first version should only need `--batch-max-pack-bytes`.

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

The most practical first heuristic is first-parent checkpointing.

For a branch tip:

1. Walk first-parent ancestry backward.
2. Sample candidate checkpoint commits.
3. Starting from the oldest candidate, estimate each batch by doing a source fetch against the previous checkpoint as `have`.
4. Pick the largest checkpoint whose pack stays under `--batch-max-pack-bytes`.
5. Repeat until the branch tip is reached.

This is only a heuristic:

- actual pack size depends on delta choices and object reuse
- merges and deep side histories can make batch sizes uneven

But it is much simpler than exact graph partitioning and good enough for a first implementation.

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

- pack-size estimation may require extra source fetches before actual execution
- checkpoint search may be slow on very deep histories
- target-side unpack/index cost may still be high even after batching, just smaller
- temp refs add cleanup and restart complexity

This is still likely worthwhile for very large initial migrations because it changes a single huge risky operation into several bounded ones.

## Recommended Phases

Phase A:

- batch branch-only bootstrap
- no tags
- temp refs required
- no resume
- manual cleanup if interrupted

Phase B:

- add resume from existing temp refs
- add better progress reporting
- add batch-size estimation metrics

Phase C:

- consider tag creation after successful branch completion
- consider whether per-ref or per-branch parallelism is worth it

Phase D:

- only then consider using similar checkpoint batching ideas for non-empty target incremental relay
