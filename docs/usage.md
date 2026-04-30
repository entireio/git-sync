# Usage

This guide covers day-to-day `gitsync` CLI usage: commands, common examples, machine-readable output, auth, and protocol behavior. For product rationale and memory model details, see [architecture.md](architecture.md). For the wire protocol walkthrough, see [protocol.md](protocol.md).

## Commands

The main commands are:

- `gitsync sync`: mirror source refs into the target
- `gitsync replicate`: overwrite target refs to match source via relay, and fail rather than materialize locally

`sync` automatically bootstraps an empty target, so the same command covers initial seeding and ongoing sync. To preview what would happen without pushing, run `gitsync plan` — it takes the same flags as `sync`, and `--mode replicate` previews a `replicate` run.

Additional commands (`bootstrap`, `probe`, `fetch`) and advanced flags are available through `gitsync --help` and the unstable library surface. They are not part of the recommended public surface.

## Examples

Run a replication that overwrites differing target refs, and fail instead of falling back to local materialization:

```bash
gitsync replicate \
  --stats \
  https://github.com/source-org/source-repo.git \
  https://github.com/target-org/target-repo.git
```

If `replicate` cannot use relay against the target, it fails and tells you to rerun with `sync`.

For very large initial migrations, add `--target-max-pack-bytes` to split the initial pack into multiple smaller batches. The same flag works on `sync`, since `sync` auto-bootstraps on empty targets:

```bash
gitsync sync \
  --target-max-pack-bytes 536870912 \
  --protocol v2 \
  -v \
  <source-url> \
  <target-url>
```

Add `--measure-memory` to any command to sample elapsed time and Go heap usage:

```bash
gitsync sync \
  --measure-memory \
  --json \
  <source-url> \
  <target-url>
```

## Sync Behavior

`sync` picks the bootstrap relay path automatically when the target is empty. For non-empty targets, safe fast-forward updates also use a relay path that streams the source pack directly into target `receive-pack` without local materialization. Anything not relay-eligible (force, prune, deletes, tag retargets) falls back to a materialized path bounded by `--materialized-max-objects`.

Sync specific branches:

```bash
gitsync sync \
  --branch main,release \
  --source-token "$GITSYNC_SOURCE_TOKEN" \
  --target-token "$GITSYNC_TARGET_TOKEN" \
  <source-url> \
  <target-url>
```

Map a source branch to a different target branch:

```bash
gitsync sync \
  --map main:stable \
  <source-url> \
  <target-url>
```

Mirror tags and prune managed target refs that disappeared from source:

```bash
gitsync sync \
  --tags \
  --prune \
  <source-url> \
  <target-url>
```

Force source-side protocol v2:

```bash
gitsync sync \
  --protocol v2 \
  <source-url> \
  <target-url>
```

## JSON Output

Add `--json` to any command to emit machine-readable output instead of the default text format.

The JSON interface is stable:

- keys use `camelCase`
- refs and hashes are serialized as strings, not raw byte arrays
- top-level keys include `plans`, `pushed`, `skipped`, `blocked`, `deleted`, `dryRun`, `protocol`, and `stats`, plus `relay`, `relayMode`, `relayReason`, `batching`, `batchCount`, `plannedBatchCount`, and `tempRefs`
- each item in `plans` includes stable string fields such as `branch`, `sourceRef`, `targetRef`, `sourceHash`, `targetHash`, `kind`, `action`, and `reason`

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

- Source-side discovery and fetch can use protocol v2 when supported. Push stays on the existing v1 `receive-pack` path. `--protocol auto` tries v2 first and falls back to v1. `--protocol v2` requires the source to negotiate v2.
- Source fetch advertises current target tip hashes as `have`, so reruns download less when source and target already share history.
- Branches are updated only when the target tip is an ancestor of the source tip, unless `--force` is set. Tags are immutable by default. Retargeting an existing tag requires `--force`. With `--prune`, managed target refs that are absent on source are deleted.
- If `sync` finds blocked refs, it exits non-zero before pushing anything.
- `--stats` adds per-service request, byte, want, have, and command counters to the output.

For the deeper protocol-level walkthrough (smart HTTP, pkt-line, capability negotiation, sideband stripping, relay framing), see [protocol.md](protocol.md).
