# git-sync

`git-sync` mirrors refs from a source remote to a target remote without creating a local checkout. It uses an in-memory `go-git` object store and talks smart HTTP directly:

- `info/refs` ref advertisement for source and target
- `upload-pack` fetch from source with target tip hashes advertised as `have`
- `receive-pack` push to target with explicit ref update commands and a streamed packfile

That keeps the target side incremental without fetching target objects into the local process first.

## Why This Exists

Git already has pieces of this problem, but not this exact tool shape.

What usually exists today:

- a full local `git clone --mirror` followed by `git push --mirror`
- host-specific import or migration features
- CI jobs or shell scripts that glue fetch and push steps together
- one-off migration tooling tied to a specific platform

What those approaches usually do not give you:

- direct remote-to-remote relay behavior
- a small standalone CLI with explicit sync semantics
- front-loaded validation and planning
- machine-readable output for automation
- one tool that covers empty-target bootstrap, normal sync, and large-repo bootstrap fallback

That is the gap `git-sync` is trying to fill.

The main value is operational:

- avoid requiring a full local mirror checkout just to move refs between remotes
- make initial seeding of large repositories cheaper and more predictable
- keep incremental sync behavior explicit and safe
- give operators and automation a stable way to inspect, plan, execute, and benchmark the same workflows

This is especially useful when:

- the target is a new hosted Git service or internal Git endpoint
- bootstrap size matters more than local developer ergonomics
- you want a repeatable machine-oriented sync primitive rather than an ad hoc migration script
- you need clearer control over mapping, pruning, force rules, and relay behavior than generic shell glue usually provides

Compared to a service that keeps persistent local clones, `git-sync` is the better fit when:

- relay is common enough that streaming source-to-target is the normal case
- avoiding persistent local repo storage is an operational advantage
- remote-to-remote efficiency matters more than full local Git generality

If you need arbitrary complex reconciliation through one always-warm local full-state model, a local-clone service is still the more general tool.

The command surface is:

- `git-sync probe`: inspect a source remote, and optionally a target remote
- `git-sync fetch`: exercise source-side fetch negotiation without pushing
- `git-sync bootstrap`: seed an empty target with create-only relay behavior
- `git-sync plan`: compute source-to-target ref actions without pushing
- `git-sync sync`: execute the planned changes against the target
- `git-sync-bench`: run repeatable benchmark scenarios against fresh empty targets

## Current scope

- Smart HTTP only
- No local working tree
- Branch mirroring by default
- Optional tag mirroring with `--tags`
- Optional exact ref mapping with `--map`
- Fast-forward safety by default
- Optional forced retargeting with `--force`
- Optional managed-ref deletion with `--prune`
- Optional transfer stats output with `--stats`
- Optional machine-readable output with `--json`
- Optional source-side Git protocol v2 for `ls-refs` and `fetch`

## Limits

- Push still uses the existing v1-style `receive-pack` path.
- Protocol v2 support currently covers source discovery and source fetch only.
- `--protocol auto` tries source-side v2 first and falls back to v1.
- `--protocol v2` requires the source remote to negotiate v2.
- Ref mapping is explicit, not wildcard-based.
- Only smart HTTP remotes are supported.
- Objects are kept in memory for the duration of the run.
- Non-relay materialized syncs are bounded by `--materialized-max-objects`, an object-count guardrail for the in-memory fallback path.

## Quick Start

```bash
go run ./cmd/git-sync sync \
  --source-token "$GITSYNC_SOURCE_TOKEN" \
  --target-token "$GITSYNC_TARGET_TOKEN" \
  https://github.com/source-org/source-repo.git \
  https://github.com/target-org/target-repo.git
```

## Commands

Plan a sync without pushing anything:

```bash
go run ./cmd/git-sync plan \
  --stats \
  https://github.com/source-org/source-repo.git \
  https://github.com/target-org/target-repo.git
```

Bootstrap an empty target without using the normal local object-store sync path:

```bash
go run ./cmd/git-sync bootstrap \
  --stats \
  https://github.com/source-org/source-repo.git \
  https://github.com/target-org/target-repo.git
```

Add `--max-pack-bytes` to abort bootstrap if the streamed source pack grows past a safety threshold:

```bash
go run ./cmd/git-sync bootstrap \
  --max-pack-bytes 104857600 \
  <source-url> \
  <target-url>
```

Add `--batch-max-pack-bytes` to split large branch bootstraps into multiple relay batches with temporary refs:

```bash
go run ./cmd/git-sync bootstrap \
  --batch-max-pack-bytes 1073741824 \
  <source-url> \
  <target-url>
```

Current batching scope is intentionally narrow:

- protocol v2 only
- branch refs are batched
- optional create-only tags are pushed after branch batches complete
- temporary refs under `refs/gitsync/bootstrap/heads/`
- resume from existing temp refs is supported when they match a planned checkpoint

This mode is intended as an advanced large-repo fallback, not the default bootstrap path. Use plain `bootstrap` first when a single streamed initial sync is acceptable.

A practical starting point is:

- `--batch-max-pack-bytes 536870912` for a conservative `512 MiB` target-side batch size
- `--batch-max-pack-bytes 1073741824` when you want fewer, larger batches and the target has more headroom

For example:

```bash
go run ./cmd/git-sync bootstrap \
  --batch-max-pack-bytes 536870912 \
  --protocol v2 \
  -v \
  <source-url> \
  <target-url>
```

Add `--measure-memory` to `bootstrap`, `sync`, `plan`, `probe`, or `fetch` to sample elapsed time and Go heap usage:

```bash
go run ./cmd/git-sync bootstrap \
  --measure-memory \
  --json \
  <source-url> \
  <target-url>
```

That is useful for one-off measurements on the same fixture or test repo.

## Benchmarking

For repeated benchmark runs, prefer the dedicated benchmark command instead of manually wrapping `git-sync` invocations:

```bash
go run ./cmd/git-sync-bench \
  --scenario bootstrap \
  --source-url /tmp/git-sync-bench/kubernetes.git \
  --repeat 3 \
  --batch-max-pack-bytes 104857600 \
  --stats \
  --json
```

`git-sync-bench` creates a fresh bare target repository for each run, executes the selected scenario in-process, and reports:

- per-run wall-clock time
- per-run `syncer.Result`
- aggregate min/avg/max wall time
- aggregate internal elapsed and heap metrics from `--measure-memory`
- relay modes observed across successful runs

If `--source-url` is a local path, it is converted to `file://...` automatically. The current scenarios are:

- `--scenario bootstrap`
- `--scenario sync`

For large-repo measurements, use a local bare mirror as the source so the benchmark reflects `git-sync` behavior rather than internet variance. See [docs/benchmarking.md](docs/benchmarking.md) for details.

## Sync Behavior

When `sync` sees that all managed target refs are absent and the run is compatible with bootstrap semantics, it automatically uses the bootstrap relay path instead of the normal decode-and-repack sync path.

`sync` also uses a narrow incremental relay path for fast-forward branch updates and tag creation when there is no prune/delete, no force, and the target does not advertise `no-thin`. This now includes multi-branch batches, branch-to-branch mappings, and create-only tags. Tag retargeting and other more complex updates still fall back to the normal local decode-and-repack path.

If `sync` falls back to the materialized path, `--materialized-max-objects` sets an explicit object-count safety bound for the in-memory object set. It is a conservative guardrail, not a precise heap-size limit.

Sync specific branches:

```bash
go run ./cmd/git-sync sync \
  --branch main,release \
  --source-token "$GITSYNC_SOURCE_TOKEN" \
  --target-token "$GITSYNC_TARGET_TOKEN" \
  <source-url> \
  <target-url>
```

Map a source branch to a different target branch:

```bash
go run ./cmd/git-sync sync \
  --map main:stable \
  <source-url> \
  <target-url>
```

Mirror tags and prune managed target refs that disappeared from source:

```bash
go run ./cmd/git-sync sync \
  --tags \
  --prune \
  <source-url> \
  <target-url>
```

Force source-side protocol v2:

```bash
go run ./cmd/git-sync sync \
  --protocol v2 \
  <source-url> \
  <target-url>
```

Probe a source remote without pushing anything:

```bash
go run ./cmd/git-sync probe \
  --stats \
  --tags \
  --protocol auto \
  <source-url>
```

Probe both source and target remotes to inspect source fetch capabilities and target `receive-pack` capabilities:

```bash
go run ./cmd/git-sync probe \
  --stats \
  <source-url> \
  <target-url>
```

Fetch from a source remote into memory without pushing anywhere:

```bash
go run ./cmd/git-sync fetch \
  --stats \
  --protocol auto \
  --branch main \
  <source-url>
```

Advertise an existing source ref as a synthetic `have` to exercise incremental negotiation:

```bash
go run ./cmd/git-sync fetch \
  --stats \
  --protocol auto \
  --branch main \
  --have-ref main \
  <source-url>
```

Dry run:

```bash
go run ./cmd/git-sync plan --stats <source-url> <target-url>
```

## JSON Output

Add `--json` to `probe`, `fetch`, `bootstrap`, `plan`, or `sync` to emit machine-readable output instead of the default text format.

The JSON interface is intentionally stable:

- keys use `snake_case`
- refs and hashes are serialized as strings, not raw byte arrays
- `probe` returns top-level keys such as `source_url`, `target_url`, `protocol`, `ref_prefixes`, `source_capabilities`, `target_capabilities`, `refs`, and `stats`
- `fetch` returns top-level keys such as `source_url`, `protocol`, `wants`, `haves`, `fetched_objects`, and `stats`
- `bootstrap`, `plan`, and `sync` return top-level keys such as `plans`, `pushed`, `skipped`, `blocked`, `deleted`, `dry_run`, `protocol`, and `stats`
- `bootstrap`, `plan`, and `sync` also expose `relay`, `relay_mode`, `relay_reason`, `batching`, `batch_count`, `planned_batch_count`, and `temp_refs`
- each item in `plans` includes stable string fields such as `branch`, `source_ref`, `target_ref`, `source_hash`, `target_hash`, `kind`, `action`, and `reason`

## Auth

For GitHub and similar providers, use basic auth with a token as the password.

Auth is resolved in this order:

- explicit CLI flags
- `GITSYNC_*` environment variables
- local `git credential fill` helper lookup for `http` and `https` remotes
- anonymous access

Relevant variables:

- `GITSYNC_SOURCE_TOKEN`
- `GITSYNC_TARGET_TOKEN`
- `GITSYNC_SOURCE_USERNAME` default: `git`
- `GITSYNC_TARGET_USERNAME` default: `git`

Bearer auth is also available:

- `GITSYNC_SOURCE_BEARER_TOKEN`
- `GITSYNC_TARGET_BEARER_TOKEN`

That means local testing against a dummy GitHub repo can reuse your regular Git credential helper setup without passing tokens on every command.

## Protocol Notes

- Source refs are listed with `GET /info/refs?service=git-upload-pack`.
- When the source supports it, the client can negotiate protocol v2 with `Git-Protocol: version=2`, then use `ls-refs` and `fetch`.
- Target refs are listed with `GET /info/refs?service=git-receive-pack`.
- The source fetch advertises current target tip hashes as `have`, so reruns download less when source and target already share history.
- Target push stays on the current `receive-pack` path.
- If a target ref does not exist, it is created.
- If a target ref already matches the source, it is skipped.
- Branches are updated only when the target tip is an ancestor of the source tip, unless `--force` is set.
- Tags are immutable by default. Retargeting an existing tag requires `--force`.
- If `--prune` is set, managed target refs that are absent on source are deleted.
- `plan` never pushes. If `sync` finds blocked refs, it exits non-zero before pushing anything.
- `--stats` adds per-service request, byte, want, have, and command counters to the output.

Push still uses the current low-level `receive-pack` path. Protocol v2 is used where it materially improves this tool: source-side ref discovery and source-side object download.

## Testing

Default suite:

```bash
env GOCACHE=/tmp/go-build go test ./...
```

Extended and environment-specific test instructions are in [docs/testing.md](docs/testing.md).

## Design Notes

`bootstrap` is the dedicated path for large initial syncs into an empty target. The goal is to relay a fetched source pack directly into target `receive-pack` instead of decoding the full object graph into local memory first.

Current architectural summary and package boundaries are in [docs/architecture.md](docs/architecture.md).

The design note is in [docs/bootstrap.md](docs/bootstrap.md).

For very large single-branch repositories, there is also a batching design and initial implementation note in [docs/bootstrap-batching.md](docs/bootstrap-batching.md).
