# Usage

This guide covers day-to-day `git-sync` CLI usage: commands, common examples, machine-readable output, auth, and protocol behavior. For product rationale and memory model details, see [architecture.md](architecture.md). For the wire protocol walkthrough, see [protocol.md](protocol.md).

## Commands

The main commands are:

- `git-sync sync`: mirror source refs into the target
- `git-sync replicate`: overwrite target refs to match source via relay, and fail rather than materialize locally

`sync` automatically bootstraps an empty target, so the same command covers initial seeding and ongoing sync. To preview what would happen without pushing, run `git-sync plan` — it takes the same flags as `sync`, and `--mode replicate` previews a `replicate` run.

Additional commands (`bootstrap`, `probe`, `fetch`) and advanced flags are available through `git-sync --help` and the unstable library surface. They are not part of the recommended public surface.

## Examples

Run a replication that overwrites differing target refs, and fail instead of falling back to local materialization:

```bash
git-sync replicate \
  --stats \
  https://github.com/source-org/source-repo.git \
  https://github.com/target-org/target-repo.git
```

If `replicate` cannot use relay against the target, it fails and tells you to rerun with `sync`.

For very large initial migrations, add `--target-max-pack-bytes` to split the initial pack into multiple smaller batches. The same flag works on `sync`, since `sync` auto-bootstraps on empty targets:

```bash
git-sync sync \
  --target-max-pack-bytes 536870912 \
  --protocol v2 \
  -v \
  <source-url> \
  <target-url>
```

Add `--measure-memory` to any command to sample elapsed time and Go heap usage:

```bash
git-sync sync \
  --measure-memory \
  --json \
  <source-url> \
  <target-url>
```

## Sync Behavior

`sync` picks the bootstrap relay path automatically when the target is empty. For non-empty targets, safe fast-forward updates also use a relay path that streams the source pack directly into target `receive-pack` without local materialization. Anything not relay-eligible (force, prune, deletes, tag retargets) falls back to a materialized path bounded by `--materialized-max-objects`.

Sync specific branches:

```bash
git-sync sync \
  --branch main,release \
  --source-token "$GITSYNC_SOURCE_TOKEN" \
  --target-token "$GITSYNC_TARGET_TOKEN" \
  <source-url> \
  <target-url>
```

Map a source branch to a different target branch:

```bash
git-sync sync \
  --map main:stable \
  <source-url> \
  <target-url>
```

Mirror tags and prune managed target refs that disappeared from source:

```bash
git-sync sync \
  --tags \
  --prune \
  <source-url> \
  <target-url>
```

Force source-side protocol v2:

```bash
git-sync sync \
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
