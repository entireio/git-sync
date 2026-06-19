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

### Bootstrap chain ordering

Batched bootstrap walks a chain of source commits and places sub-pack
boundaries (checkpoints) along it, so each push fits under
`--target-max-pack-bytes`. Two orderings are available via
`--bootstrap-strategy`:

- `first-parent` (default) walks only the first-parent backbone.
  Each step from one checkpoint to the next is the smallest unit the
  planner can subdivide.
- `topo` includes every reachable commit in topological order
  (parents before children, hash-tie-broken for stable resume).

The default is fine for most repos. `topo` is for the case where a
single first-parent step pulls in a large side-branch ancestry and
cannot be subdivided. Concrete example: assume the target's pack-body
limit fits about two commits' worth of objects.

```
     root ── A ──────────────── M ── tip       (first-parent backbone)
              \                /
               S1 ─ S2 ─ S3 ─ S4              (side branch, merged at M)
```

Under `first-parent`, the planner only knows `root → A → M → tip`:

```
checkpoint 1:  root → A   pack {A}                  ✅ fits
checkpoint 2:  A → M      pack {S1, S2, S3, S4, M}  ❌ 5 commits, too big
checkpoint 3:  M → tip    pack {tip}                ✅ fits
```

The `A → M` step is one indivisible unit because no checkpoint can
land on `S2` — `S2` isn't on the backbone. The bootstrap fails: the
pack exceeds the limit and can't be split further.

Under `topo`, every reachable commit is a candidate checkpoint:

```
chain:  root → A → S1 → S2 → S3 → S4 → M → tip

checkpoint 1:  root → A    pack {A}        ✅
checkpoint 2:  A → S2      pack {S1, S2}   ✅
checkpoint 3:  S2 → S4     pack {S3, S4}   ✅
checkpoint 4:  S4 → M      pack {M}        ✅ tiny — merge content already pushed
checkpoint 5:  M → tip     pack {tip}      ✅
```

Trade-offs:

- **Cost**: more source-side enumeration (chain length grows with
  every reachable commit, not just the backbone). For a linear repo
  the two strategies are identical; for a heavily-merged repo `topo`
  walks every side-branch commit too.
- **Server requirement**: under `topo`, successive checkpoints aren't
  always in an ancestor-descendant relationship (topological order
  can interleave parallel branches), so the internal
  `refs/gitsync/bootstrap/heads/<branch>` temp ref may receive
  non-fast-forward updates between checkpoints. The temp ref is
  internal scaffolding — user-visible refs (`refs/heads`, `refs/tags`)
  only get a single fast-forward update at cutover — but targets that
  enforce `receive.denyNonFastforwards` across all refs (rather than
  just `refs/heads`) will reject those temp-ref updates and fail the
  bootstrap. Major hosts (GitHub, GitLab, Bitbucket, Cloudflare) do
  not enable this by default.

```bash
git-sync sync \
  --target-max-pack-bytes 100000000 \
  --bootstrap-strategy topo \
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

### SSH remotes

`git-sync` also supports SSH remotes. Accepted forms include:

- `ssh://git@example.com/org/repo.git`
- `git@example.com:org/repo.git`
- `git+ssh://example.com/org/repo.git`

SSH transport shells out to the local `ssh` binary, so host aliases,
`IdentityFile`, agent-backed keys, and other `~/.ssh/config` behavior come
from your existing SSH setup rather than separate `git-sync` flags.

`git-sync` runs SSH with `BatchMode=yes`, which avoids interactive password or
host-key prompts during syncs. On first contact with a host, add it to
`known_hosts` ahead of time or configure `StrictHostKeyChecking=accept-new`
for that host in your SSH config.

Current limitation: `--progress` and `--show-stats` do not yet include
byte-counted SSH transfer metrics, so `--progress` and `--stats` omit
SSH-side throughput.

If `ssh` is not available on `PATH`, `git-sync` fails early with a clear
`locate ssh binary` error before contacting either remote.

### Remote-helper schemes

For any URL scheme `git-sync` has no native transport for, it looks for a git
remote helper named `git-remote-<scheme>` on `PATH` — the same mechanism `git`
itself uses for custom schemes. If one is installed, `git-sync` delegates auth
and the network round trips to it while still driving the smart protocol over
the helper's `stateless-connect` bridge.

The motivating case is Entire's own `entire://` URLs:

```bash
git-sync replicate --all-refs \
  https://github.com/source-org/source-repo.git \
  entire://cluster-host/et/project/repo
```

This requires `git-remote-entire` (installed by the Entire CLI) on `PATH`.
Authentication — including the Entire context model and OAuth token refresh —
is handled entirely by the helper, so no `--target-token` is needed when you're
already logged in via the CLI. Both fetch (upload-pack, protocol v2) and push
(receive-pack) work through the helper.

`http`/`https`/`ssh` are never routed to a helper: `git-sync` always uses its
own optimized transports for those, even though `git` ships
`git-remote-http(s)`.

Like SSH, the helper bridge has no per-request byte accounting, so `--stats`
omits helper-side throughput (the other side's throughput still prints).

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

Mirror every ref namespace (notes, pulls, custom) on a best-effort basis:

```bash
git-sync sync \
  --all-refs \
  <source-url> \
  <target-url>
```

`--all-refs` broadens the source ref discovery from `refs/heads/`+`refs/tags/`
to every `refs/*` namespace (branches, tags, `refs/notes/*`, `refs/pull/*`,
custom refs) and lets ref mappings target arbitrary namespaces. Tag
inclusion is implied — `RefScope.AllRefs` covers tags at the library level,
so `--tags` does not need to be combined with `--all-refs`.

For `sync` and `bootstrap` the flag also turns on best-effort failure
handling: when the target's `receive-pack` rejects an individual ref (e.g.
GitHub refusing writes to `refs/pull/*` hidden refs), the rejected ref
appears in the result with `action=warn` and the server's reason instead
of failing the whole sync. Pack-level transport or unpack failures remain
fatal, and so do source-side `upload-pack` failures — if the source server
advertises a hidden ref but refuses to serve a `want` for its tip (Gerrit
`refs/changes/*` is a common case), the fetch errors out with no per-ref
warn granularity. BestEffort only covers target-side `receive-pack`.

Trim noisy namespaces with `--exclude-ref-prefix` (repeatable). The
common case is mirroring an open-source GitHub repo where `--all-refs`
would otherwise pull every PR's fork commits via `refs/pull/*`:

```bash
git-sync sync \
  --all-refs \
  --exclude-ref-prefix refs/pull/ \
  <source-url> \
  <target-url>
```

`--exclude-ref-prefix` subtracts from auto-discovery (branches, tags, and
`--all-refs` namespaces) and from prune scope, so excluded refs are left
alone entirely: not pulled from source, not pushed to target, not pruned
from target. Explicit `--map` entries are not subject to this filter.

`replicate --all-refs` broadens the same scope but does NOT enable
best-effort. Replicate's contract is "target refs match source"; downgrading
rejected refs to warnings would let partial mirrors exit successfully,
which contradicts the command. Use `sync --all-refs` if you want
best-effort completeness against hostile targets.

`sync --all-refs` blocks updates to non-branch refs (notes, pulls, custom
namespaces) by default — those refs don't generally form fast-forward
chains, so the same `--force-with-lease` opt-in that retargets tags is
required to update them. `replicate` doesn't run that check; its overwrite
contract covers other-kind refs without a force flag.

`SyncPolicy.BestEffort` is independent of scope and can be set without
`AllRefs` if a library caller wants per-ref warn semantics on a narrower
scope.

Force source-side protocol v2:

```bash
git-sync sync \
  --protocol v2 \
  <source-url> \
  <target-url>
```

## Force Updates and the Per-Run Lease

Non-fast-forward updates and tag retargets are opt-in. git-sync exposes two
flags that mirror `git push`'s force semantics:

- **`--force-with-lease`** — allow non-fast-forward updates, but include the
  target tip captured at session start as the push command's expected-old
  value. If another writer moves the target between session start and the
  push, receive-pack rejects the update with a "remote ref does not match
  expected old value" error and the sync fails without clobbering the racing
  write. The lease window is one sync run; git-sync keeps no state between
  runs.
- **`--force-blind`** — allow non-fast-forward updates and zero out the
  expected-old, telling receive-pack to overwrite regardless of current
  target value. Matches `git push --force` semantics. Use this when the
  target was edited out-of-band and you intend to overwrite whatever is
  there.

The two flags are mutually exclusive. Without either, divergent or
non-ancestor refs are reported as blocked and the sync exits non-zero
before any push, so the lease check is a second line of defense against
races for users who opt into non-fast-forward updates.

`bootstrap` and `replicate` do not accept force flags. Bootstrap seeds an
empty target where every ref is a create. Replicate's contract is
source-authoritative overwrite: divergent branches and tags are retargeted
against the source unconditionally, so there is no fast-forward gate for a
force flag to opt out of.

The pre-0.5 `--force` flag is removed. Its semantics were lease-protected
(it never sent a zero expected-old), so the closest direct replacement is
`--force-with-lease`. `--force-blind` is new behavior with no pre-0.5
analog.

## HEAD / Default Branch

git-sync surfaces the source's symref HEAD target — the source's default
branch — in result output:

- JSON: `execution.sourceHead` (sync / plan / replicate / bootstrap); `sourceHead` (probe).
- Human: a `source-head: <ref>` line.

```
$ git-sync probe --json <source-url> | jq .sourceHead
"refs/heads/main"
```

**Bootstrap pushes the source HEAD's branch first.** When initializating a new 
repository, the default branch is based on the first push to a fresh repo (GitHub, GitLab,
others), this makes the mirror's default branch match the source
without any manual step. No flag needed — the ordering is always applied.

git's wire protocol has no command for updating a remote symref, so for
hosts that *don't* infer the default from first-push (raw bare repos,
some self-hosted setups), the default branch has to be set out of band:

1. **Match the default at init time** — `git init --bare --initial-branch=<source-default>` on the target before the first sync.
2. **Set HEAD post-sync** — `git symbolic-ref HEAD refs/heads/<source-default>` on the bare repo, or the host's API/UI (GitHub `PATCH /repos/{owner}/{repo}` with `default_branch`; GitLab `PUT /projects/:id` with `default_branch`).

## JSON Output

Add `--json` to any command to emit machine-readable output instead of the default text format.

The JSON interface is stable:

- keys use `camelCase`
- refs and hashes are serialized as strings, not raw byte arrays
- top-level keys include `plans`, `pushed`, `skipped`, `blocked`, `deleted`, `warned`, `dryRun`, `protocol`, and `stats`, plus `relay`, `relayMode`, `relayReason`, `batching`, `batchCount`, `plannedBatchCount`, and `tempRefs`
- each item in `plans` includes stable string fields such as `branch`, `sourceRef`, `targetRef`, `sourceHash`, `targetHash`, `kind`, `action`, and `reason`
- `execution.sourceHead` (sync/plan/replicate/bootstrap) and `sourceHead` (probe) carry the source's symref HEAD target when advertised

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
- Branches are updated only when the target tip is an ancestor of the source tip, unless `--force-with-lease` or `--force-blind` is set. Tags are immutable by default; retargeting an existing tag requires one of the force flags. With `--prune`, managed target refs that are absent on source are deleted.
- If `sync` finds blocked refs, it exits non-zero before pushing anything.
- `--stats` adds per-service request, byte, want, have, and command counters to the output.

For the deeper protocol-level walkthrough (smart HTTP, pkt-line, capability negotiation, sideband stripping, relay framing), see [protocol.md](protocol.md).
