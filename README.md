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

`git-sync` is also a Go library. Use `entire.io/entire/gitsync` for the stable embedding surface (`Probe`, `Plan`, `Sync`, `Replicate`, typed results, auth and HTTP injection). `entire.io/entire/gitsync/unstable` exposes advanced controls (`Bootstrap`, `Fetch`, batching knobs, heap measurement) and is not stable.

## Installation

Requires Go 1.26 or newer.

Install the latest release with `go install`:

```bash
go install entire.io/entire/gitsync/cmd/git-sync@latest
```

This drops a `git-sync` binary into `$(go env GOPATH)/bin`. Make sure that directory is on your `PATH`.

Or build from source:

```bash
git clone https://github.com/entireio/git-sync.git
cd git-sync
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

## FAQ

### Does it sync complete Git history or only perform a shallow/partial sync?

`git-sync` syncs the complete Git object history required for the selected refs. It does not create a shallow clone. Some planning paths may use filtered fetches, but the target receives the full objects needed for valid refs.

### Is it just refs, or objects as well?

Objects as well. Refs are what `git-sync` plans and updates, but it also transfers the commits, trees, blobs, and tags needed for those refs to exist on the target.

### Is it bidirectional?

No. `git-sync` is one-way: source remote to target remote. To go the other way you'd run a second invocation with the endpoints swapped.

### Does it support create, update, and delete actions?

Yes. It supports creating refs, updating refs, force updates with `--force`, and deleting managed refs with `--prune`. `replicate` can overwrite target refs, but it is relay-only and more restrictive than `sync`.

### How does it scale?

`git-sync` has two transfer paths:

- **Relay** — pack data streams from source `upload-pack` directly into target `receive-pack`. The local process holds no object graph, so memory stays bounded regardless of repo size. Used when the target supports relay.
- **Materialized fallback** — when relay isn't available, `git-sync` fetches the needed objects into an in-memory `go-git` store, plans, then encodes and pushes a packfile. Memory scales with the diff being pushed and is guarded by an explicit object-count limit. Bootstrap can batch large initial syncs to keep this bounded.

Planning itself is cheap: ref-only round-trips, plus a `filter tree:0` fetch for ancestry checks when the source advertises filter support.

### How long does it take for a medium-sized repo?

It depends on repository size, network speed, and whether the relay path is available. As a rule of thumb, the relay path is bounded by source pack generation + network transfer + target receive-pack time — roughly the time of a `git clone` from the source plus a `git push` of the same pack. The materialized fallback adds local memory work for objects that need inspection.

For concrete numbers on your own setup, run the included benchmark tool against a representative repo; see [docs/testing.md](docs/testing.md#benchmarking).

### Does it support SSH?

No. `git-sync` supports smart HTTP/HTTPS only.

### Does it run as a daemon or watch for changes?

No. `git-sync` is a one-shot CLI/library operation. To sync on a schedule or in response to events, run it from cron, CI, a worker, or another service.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md), [SECURITY.md](SECURITY.md), and [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md).
