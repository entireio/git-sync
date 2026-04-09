# git-sync

`git-sync` mirrors refs from a source remote to a target remote without creating a local checkout. It uses an in-memory `go-git` object store and talks smart HTTP directly:

- `info/refs` ref advertisement for source and target
- `upload-pack` fetch from source with target tip hashes advertised as `have`
- `receive-pack` push to target with explicit ref update commands and a streamed packfile

That keeps the target side incremental without fetching target objects into the local process first.

The command surface is:

- `git-sync probe`: inspect a source remote, and optionally a target remote
- `git-sync fetch`: exercise source-side fetch negotiation without pushing
- `git-sync bootstrap`: seed an empty target with create-only relay behavior
- `git-sync plan`: compute source-to-target ref actions without pushing
- `git-sync sync`: execute the planned changes against the target

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

## Usage

```bash
go run ./cmd/git-sync sync \
  --source-token "$GITSYNC_SOURCE_TOKEN" \
  --target-token "$GITSYNC_TARGET_TOKEN" \
  https://github.com/source-org/source-repo.git \
  https://github.com/target-org/target-repo.git
```

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

Add `--measure-memory` to `bootstrap`, `sync`, `plan`, `probe`, or `fetch` to sample elapsed time and Go heap usage:

```bash
go run ./cmd/git-sync bootstrap \
  --measure-memory \
  --json \
  <source-url> \
  <target-url>
```

That is the intended way to compare the bootstrap relay path against the normal sync path on the same fixture or test repo.

When `sync` sees that all managed target refs are absent and the run is compatible with bootstrap semantics, it automatically uses the bootstrap relay path instead of the normal decode-and-repack sync path.

`sync` also uses a narrow incremental relay path for fast-forward branch updates and tag creation when there is no prune/delete, no force, and the target does not advertise `no-thin`. This now includes multi-branch batches, branch-to-branch mappings, and create-only tags. Tag retargeting and other more complex updates still fall back to the normal local decode-and-repack path.

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

Add `--json` to `probe`, `fetch`, `bootstrap`, `plan`, or `sync` to emit machine-readable output instead of the default text format.

The JSON interface is intentionally stable:

- keys use `snake_case`
- refs and hashes are serialized as strings, not raw byte arrays
- `probe` returns top-level keys such as `source_url`, `target_url`, `protocol`, `ref_prefixes`, `source_capabilities`, `target_capabilities`, `refs`, and `stats`
- `fetch` returns top-level keys such as `source_url`, `protocol`, `wants`, `haves`, `fetched_objects`, and `stats`
- `bootstrap`, `plan`, and `sync` return top-level keys such as `plans`, `pushed`, `skipped`, `blocked`, `deleted`, `dry_run`, `protocol`, and `stats`
- each item in `plans` includes stable string fields such as `branch`, `source_ref`, `target_ref`, `source_hash`, `target_hash`, `kind`, `action`, and `reason`

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

## Auth

For GitHub and similar providers, use basic auth with a token as the password.

Auth is resolved in this order:

- explicit CLI flags
- `GITSYNC_*` environment variables
- local `git credential fill` helper lookup for `http` and `https` remotes
- anonymous access

- `GITSYNC_SOURCE_TOKEN`
- `GITSYNC_TARGET_TOKEN`
- `GITSYNC_SOURCE_USERNAME` default: `git`
- `GITSYNC_TARGET_USERNAME` default: `git`

Bearer auth is also available:

- `GITSYNC_SOURCE_BEARER_TOKEN`
- `GITSYNC_TARGET_BEARER_TOKEN`

That means local testing against a dummy GitHub repo can reuse your regular Git credential helper setup without passing tokens on every command.

## Behavior

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

## Why Push Stays V1

Protocol v2 is used where it materially improves this tool: source-side ref discovery and source-side object download.

Push remains on the existing low-level `receive-pack` path for two reasons:

- The tool already builds exact ref update commands and streams the outgoing packfile directly, so push-side control was already good before v2 support.
- The main transfer and negotiation win is on the source side. That is where `ls-refs` and `fetch` reduce unnecessary work.

In other words, this project uses protocol v2 where it changes the fetch/list behavior in a useful way, and keeps the current push path where switching protocols would mostly add complexity without a comparable payoff.

## Testing

The default test suite uses in-process smart HTTP servers and does not require a local listener:

```bash
env GOCACHE=/tmp/go-build go test ./...
```

There is also an optional end-to-end write test against the system `git-http-backend`:

```bash
env GOCACHE=/tmp/go-build GITSYNC_E2E_GIT_HTTP_BACKEND=1 go test ./internal/syncer -run TestRun_GitHTTPBackendSync -v
```

That path exercises real smart HTTP fetch and push with a local bare source repo and a local bare target repo.

There is also an optional live read-only smoke against the public Linux repository:

```bash
env GOCACHE=/tmp/go-build GITSYNC_E2E_LIVE_LINUX=1 go test ./internal/syncer -run TestFetch_LiveLinuxSource -v
```

That is useful for large-source protocol and memory measurement checks without requiring a writable remote.

## Planned Bootstrap Path

There is a planned `bootstrap` command path for large initial syncs into an empty target. The intent is to relay a fetched source pack directly into target `receive-pack` instead of decoding the full object graph into local memory first.

The design note is in [docs/bootstrap.md](/Users/soph/Work/entire/devenv/git-sync/docs/bootstrap.md).
