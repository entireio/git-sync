# git-sync

`git-sync` mirrors refs from a source remote to a target remote without creating a local checkout. It uses an in-memory `go-git` object store and talks smart HTTP directly:

- `info/refs` ref advertisement for source and target
- `upload-pack` fetch from source with target tip hashes advertised as `have`
- `receive-pack` push to target with explicit ref update commands and a streamed packfile

That keeps the target side incremental without fetching target objects into the local process first.

## Why This Exists

Mirroring Git data between remotes usually means a local mirror clone followed by a mirror push. That's fine for small repos but turns a remote-to-remote operation into a local storage problem at scale, and shell glue around `git fetch` / `git push` tends to skip planning and structured output.

`git-sync` fills that gap. It streams source packs directly into target `receive-pack` when it can, plans every action before pushing, and emits typed JSON for automation.

For when to use it (and when not), see [docs/architecture.md](docs/architecture.md).

## Commands

The main commands are:

- `git-sync sync`: mirror source refs into the target
- `git-sync replicate`: overwrite target refs to match source via relay, and fail rather than materialize locally

`sync` automatically bootstraps an empty target, so the same command covers initial seeding and ongoing sync. To preview what would happen without pushing, run `git-sync plan` — it takes the same flags as `sync`, and `--mode replicate` previews a `replicate` run.

For command examples, JSON output, auth, protocol flags, and advanced command notes, see [docs/usage.md](docs/usage.md).

## Library API

`git-sync` is also a Go library. Use `entire.io/entire/git-sync` for the stable embedding surface (`Probe`, `Plan`, `Sync`, `Replicate`, typed results, auth and HTTP injection). `entire.io/entire/git-sync/unstable` exposes advanced controls (`Bootstrap`, `Fetch`, batching knobs, heap measurement) and is not stable.

## Installation

Requires Go 1.26 or newer.

Install the latest release with `go install`:

```bash
go install entire.io/entire/git-sync/cmd/git-sync@latest
```

This drops a `git-sync` binary into `$(go env GOPATH)/bin`. Make sure that directory is on your `PATH`.

Or build from source:

```bash
git clone https://github.com/entireio/gitsync.git
cd gitsync
go build -o git-sync ./cmd/git-sync
```

## Quick Start

```bash
git-sync sync \
  --source-token "$GITSYNC_SOURCE_TOKEN" \
  --target-token "$GITSYNC_TARGET_TOKEN" \
  https://github.com/source-org/source-repo.git \
  https://github.com/target-org/target-repo.git
```

## Sync Behavior

`sync` picks the bootstrap relay path automatically when the target is empty. For non-empty targets, safe fast-forward updates also use a relay path that streams the source pack directly into target `receive-pack` without local materialization. Anything not relay-eligible (force, prune, deletes, tag retargets) falls back to a materialized path bounded by `--materialized-max-objects`.

For branch filtering, ref mapping, tags, pruning, protocol selection, JSON output, and auth details, see [docs/usage.md](docs/usage.md).

## Testing

Default suite:

```bash
env GOCACHE=/tmp/go-build go test ./...
```

Extended and environment-specific test instructions are in [docs/testing.md](docs/testing.md).

## Documentation

- [docs/usage.md](docs/usage.md) — CLI commands, examples, sync behavior, JSON output, auth, protocol notes
- [docs/architecture.md](docs/architecture.md) — product rationale, package layout, operation modes vs transfer modes, memory model
- [docs/protocol.md](docs/protocol.md) — smart HTTP, pkt-line, capability negotiation, sideband, relay framing
- [docs/testing.md](docs/testing.md) — test suites and integration coverage

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md), [SECURITY.md](SECURITY.md), and [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md).
