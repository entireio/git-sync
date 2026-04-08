# git-sync

`git-sync` mirrors refs from a source remote to a target remote without creating a local checkout. It uses an in-memory `go-git` object store and talks smart HTTP directly:

- `info/refs` ref advertisement for source and target
- `upload-pack` fetch from source with target tip hashes advertised as `have`
- `receive-pack` push to target with explicit ref update commands and a streamed packfile

That keeps the target side incremental without fetching target objects into the local process first.

The command surface is:

- `git-sync probe`: inspect a source remote, and optionally a target remote
- `git-sync fetch`: exercise source-side fetch negotiation without pushing
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
go run ./cmd/git-sync sync --dry-run --stats <source-url> <target-url>
```

## Auth

For GitHub and similar providers, use basic auth with a token as the password.

- `GITSYNC_SOURCE_TOKEN`
- `GITSYNC_TARGET_TOKEN`
- `GITSYNC_SOURCE_USERNAME` default: `git`
- `GITSYNC_TARGET_USERNAME` default: `git`

Bearer auth is also available:

- `GITSYNC_SOURCE_BEARER_TOKEN`
- `GITSYNC_TARGET_BEARER_TOKEN`

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
- If any ref would be blocked and `--dry-run` is not set, the command exits non-zero before pushing anything.
- `--stats` adds per-service request, byte, want, have, and command counters to the output.

## Why Push Stays V1

Protocol v2 is used where it materially improves this tool: source-side ref discovery and source-side object download.

Push remains on the existing low-level `receive-pack` path for two reasons:

- The tool already builds exact ref update commands and streams the outgoing packfile directly, so push-side control was already good before v2 support.
- The main transfer and negotiation win is on the source side. That is where `ls-refs` and `fetch` reduce unnecessary work.

In other words, this project uses protocol v2 where it changes the fetch/list behavior in a useful way, and keeps the current push path where switching protocols would mostly add complexity without a comparable payoff.
