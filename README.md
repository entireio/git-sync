# git-sync

`git-sync` mirrors refs from a source remote to a target remote without creating a local checkout. It uses an in-memory `go-git` object store and talks smart HTTP directly:

- `info/refs` ref advertisement for source and target
- `upload-pack` fetch from source with target tip hashes advertised as `have`
- `receive-pack` push to target with explicit ref update commands and a streamed packfile

That keeps the target side incremental without fetching target objects into the local process first.

## Why This Exists

The usual ways to mirror Git data between remotes are awkward at exactly the layer operators tend to need: a local `git clone --mirror` followed by `git push --mirror` turns a remote-to-remote movement into a local storage and bandwidth problem; host-specific migration features aren't portable across providers; and shell scripts around `git fetch` and `git push` usually lack planning, explicit policy, and machine-readable output.

`git-sync` is meant to be the missing middle layer: a provider-agnostic, remote-to-remote primitive that streams packs directly source-to-target when possible, front-loads validation, exposes typed JSON output, and covers both empty-target bootstrap and incremental sync with one tool. It's the right fit when relay is common enough to be the normal case rather than an exceptional optimization, and when avoiding persistent local repo storage is itself an operational advantage.

For when to use it (and when not), how it compares to local-clone services, and the operation-mode and transfer-mode model, see [docs/architecture.md](docs/architecture.md).

## Commands

The command surface is:

- `git-sync probe`: inspect a source remote, and optionally a target remote
- `git-sync plan`: compute source-to-target ref actions without pushing, with `--mode sync|replicate`
- `git-sync sync`: execute the planned changes against the target
- `git-sync replicate`: execute source-authoritative relay-only replication against the target

`sync` auto-selects the bootstrap relay path on empty targets, so the same command covers initial seeding and ongoing sync.

## Library API

`git-sync` now has a two-tier Go API:

- `entire.io/entire/gitsync`
  - stable embedding surface for queue workers and other external callers
  - typed `Probe`, `Plan`, `Sync`, and `Replicate` requests/results
  - injected auth and HTTP client support
- `entire.io/entire/gitsync/unstable`
  - explicitly non-stable surface for first-party tooling and advanced controls
  - includes `Bootstrap`, `Fetch`, batching and measurement knobs, and CLI-oriented execution options

If you are embedding `git-sync` outside this repo, prefer `gitsync`. The CLI and benchmark command use `unstable` because they still need direct access to advanced engine controls that are intentionally not part of the stable API.

The stable `gitsync` results are shaped for workers:

- `Refs`
  - per-ref outcomes
- `Counts`
  - aggregate applied/skipped/blocked/deleted counts
- `Execution`
  - execution mode, protocol, relay summary, and batch summary

See [docs/embedding.md](docs/embedding.md) for worker-oriented guidance.

## Current scope

- Smart HTTP only
- No local working tree
- Branch mirroring by default
- Optional tag mirroring with `--tags`
- Optional exact ref mapping with `--map`
- Fast-forward safety by default
- Optional forced retargeting with `--force`
- Optional source-authoritative relay-only replication with `replicate` / `plan --mode replicate`
- Optional managed-ref deletion with `--prune`
- Optional transfer stats output with `--stats`
- Optional machine-readable output with `--json`
- Optional source-side Git protocol v2 for `ls-refs` and `fetch`

## Limitations

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

## Examples

Plan a sync without pushing anything:

```bash
go run ./cmd/git-sync plan \
  --stats \
  https://github.com/source-org/source-repo.git \
  https://github.com/target-org/target-repo.git
```

Plan a source-authoritative replication without pushing anything:

```bash
go run ./cmd/git-sync plan \
  --mode replicate \
  --stats \
  https://github.com/source-org/source-repo.git \
  https://github.com/target-org/target-repo.git
```

Execute relay-only replication that overwrites differing managed refs and fails instead of materializing locally:

```bash
go run ./cmd/git-sync replicate \
  --stats \
  https://github.com/source-org/source-repo.git \
  https://github.com/target-org/target-repo.git
```

If `replicate` cannot use relay against the target, it fails and tells you to rerun with `sync`.

For very large initial migrations, add `--target-max-pack-bytes` to split the initial pack into multiple relay batches with temporary refs. `sync` auto-bootstraps on empty targets, so the same flag works without invoking a separate command:

```bash
go run ./cmd/git-sync sync \
  --target-max-pack-bytes 536870912 \
  --protocol v2 \
  -v \
  <source-url> \
  <target-url>
```

Add `--measure-memory` to any command to sample elapsed time and Go heap usage:

```bash
go run ./cmd/git-sync sync \
  --measure-memory \
  --json \
  <source-url> \
  <target-url>
```

## Sync Behavior

`sync` auto-selects the bootstrap relay path when the target has no managed refs and the run matches bootstrap semantics. It also has a narrow incremental relay path for safe fast-forward updates that streams the source pack directly into target `receive-pack` without local materialization. Updates that aren't relay-eligible (force, prune, deletes, tag retargets) fall back to a materialized path bounded by `--materialized-max-objects`. See [docs/incremental-relay.md](docs/incremental-relay.md) and [docs/bootstrap.md](docs/bootstrap.md) for details.

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

Dry run:

```bash
go run ./cmd/git-sync plan --stats <source-url> <target-url>
```

## JSON Output

Add `--json` to `probe`, `plan`, or `sync` to emit machine-readable output instead of the default text format.

The JSON interface is intentionally stable:

- keys use `camelCase`
- refs and hashes are serialized as strings, not raw byte arrays
- `probe` returns top-level keys such as `sourceUrl`, `targetUrl`, `protocol`, `refPrefixes`, `sourceCapabilities`, `targetCapabilities`, `refs`, and `stats`
- `plan` and `sync` return top-level keys such as `plans`, `pushed`, `skipped`, `blocked`, `deleted`, `dryRun`, `protocol`, and `stats`, and also expose `relay`, `relayMode`, `relayReason`, `batching`, `batchCount`, `plannedBatchCount`, and `tempRefs`
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

- Source-side discovery and fetch can use protocol v2 when supported; push stays on the existing v1 `receive-pack` path. `--protocol auto` tries v2 first and falls back to v1; `--protocol v2` requires the source to negotiate v2.
- Source fetch advertises current target tip hashes as `have`, so reruns download less when source and target already share history.
- Branches are updated only when the target tip is an ancestor of the source tip, unless `--force` is set. Tags are immutable by default; retargeting an existing tag requires `--force`. If `--prune` is set, managed target refs that are absent on source are deleted.
- `plan` never pushes. If `sync` finds blocked refs, it exits non-zero before pushing anything.
- `--stats` adds per-service request, byte, want, have, and command counters to the output.

For the deeper protocol-level walkthrough (smart HTTP, pkt-line, capability negotiation, sideband stripping, relay framing), see [docs/protocol.md](docs/protocol.md).

## Testing

Default suite:

```bash
env GOCACHE=/tmp/go-build go test ./...
```

Extended and environment-specific test instructions are in [docs/testing.md](docs/testing.md).

## Documentation

- [docs/architecture.md](docs/architecture.md) — product rationale, package layout, operation modes vs transfer modes, memory model
- [docs/protocol.md](docs/protocol.md) — smart HTTP, pkt-line, capability negotiation, sideband, relay framing
- [docs/bootstrap.md](docs/bootstrap.md) — empty-target relay
- [docs/bootstrap-batching.md](docs/bootstrap-batching.md) — checkpoint batching for very large initial migrations
- [docs/incremental-relay.md](docs/incremental-relay.md) — narrow relay fast path inside `sync`
- [docs/replicate.md](docs/replicate.md) — source-authoritative relay-only overwrite mode
- [docs/embedding.md](docs/embedding.md) — using `git-sync` as a Go library
- [docs/benchmarking.md](docs/benchmarking.md) — `git-sync-bench` usage
- [docs/testing.md](docs/testing.md) — test suites and integration coverage

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md), [SECURITY.md](SECURITY.md), and [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md).
