# git-sync

`git-sync` mirrors refs from a source remote to a target remote without creating a local checkout. It uses an in-memory `go-git` object store and talks smart HTTP directly:

- `info/refs` ref advertisement for source and target
- `upload-pack` fetch from source with target tip hashes advertised as `have`
- `receive-pack` push to target with explicit ref update commands and a streamed packfile

That keeps the target side incremental without fetching target objects into the local process first.

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

## Limits

- Protocol v2 is not implemented yet. `--protocol auto` currently resolves to v1.
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
- Target refs are listed with `GET /info/refs?service=git-receive-pack`.
- The source fetch advertises current target tip hashes as `have`, so reruns download less when source and target already share history.
- If a target ref does not exist, it is created.
- If a target ref already matches the source, it is skipped.
- Branches are updated only when the target tip is an ancestor of the source tip, unless `--force` is set.
- Tags are immutable by default. Retargeting an existing tag requires `--force`.
- If `--prune` is set, managed target refs that are absent on source are deleted.
- If any ref would be blocked and `--dry-run` is not set, the command exits non-zero before pushing anything.
- `--stats` adds per-service request, byte, want, have, and command counters to the output.
