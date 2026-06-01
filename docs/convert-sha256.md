# SHA1 → SHA256 Conversion

`git-sync convert-sha256` is a one-off migration command that fetches a pack
from a SHA1 HTTP source and writes a new SHA256 bare repository on disk.
Every reachable object is re-hashed under SHA256 and tree, commit, and tag
references are rewritten accordingly. The command does not push to a
remote, does not modify the source, and is meant to run once per repo.
SHA256 hashes have no relation to the original SHA1 hashes beyond a
mapping the command can optionally emit.

## Quick Start

```bash
git-sync convert-sha256 \
  https://github.com/source-org/source-repo.git \
  /path/to/out.git
```

The target directory must not exist or must be empty. The result is a bare
repository with `extensions.objectformat = sha256` and a
`refs/notes/sha1-origin` ref recording each commit's pre-conversion SHA1.

Scope is fixed: every branch and every tag on the source is always
converted. Pass `--all-refs` to also include `refs/notes/*` and other
custom namespaces; pair with `--exclude-ref-prefix` to subtract specific
namespaces. Server-internal pull/merge-request refs (`refs/pull/*`,
`refs/pull-requests/*`, `refs/merge-requests/*`) are excluded even under
`--all-refs` — see [Sharp Edges](#sharp-edges). Pass `--include-pull-refs`
to convert them anyway.

For a private source, pass the token via the environment so it isn't
exposed in `ps`:

```bash
GITSYNC_SOURCE_TOKEN=ghp_xxx git-sync convert-sha256 \
  https://github.com/source-org/private-repo.git \
  /path/to/out.git
```

## What It Does

1. Probes the source via smart HTTP and lists every in-scope ref.
2. Fetches a single self-contained pack via `upload-pack` into a
   temporary on-disk SHA1 bare repo (cleaned up at the end unless
   `--keep-source-objects` is passed).
3. Discovers every reachable object — walking trees, commits, and tags
   — and records each one's SHA1 and object type. Submodule gitlinks
   are checked here; unresolvable ones fail-fast before any output is
   written.
4. Initializes the target as a bare SHA256 repository
   (`git init --object-format=sha256` equivalent).
5. Translates every reachable object in topological order via memoized
   DFS:
   - **Blobs**: re-hashed under SHA256; content unchanged.
   - **Trees**: each entry's hash translated.
   - **Commits**: `tree` and `parent` hashes translated; GPG signatures
     and `mergetag` headers dropped; in-scope SHA1 references in the
     message are translated first and then substituted.
   - **Tags**: target hash translated; signatures dropped; message
     hashes rewritten the same way.
6. Writes refs at the translated tip hashes; points HEAD at the
   source's advertised HEAD when it was converted, else `main`/`master`,
   else the first branch alphabetically (a tags-only conversion leaves
   HEAD at the init default); builds `refs/notes/sha1-origin` (unless
   `--no-origin-notes`); emits the `--write-mapping` TSV (if requested).

## Side Outputs

The conversion deliberately decouples SHA1 from SHA256 — two runs of
this tool against the same source produce SHA256 hashes that share
nothing with the originals. Three on-ramps help bridge the gap.

### Inline message rewriting (default on)

Commit and tag messages are scanned for 7-to-40-character hex runs.
When a run uniquely matches a commit or tag SHA1 in the reachable set,
it is replaced with the full SHA256 hex:

```
Reverts: a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0    →    full SHA256
Cherry-picked from a1b2c3d                            →    full SHA256
```

Two properties make this robust:

- **Uniqueness is decided against the reachable set, not the in-flight
  mapping.** The discovery pass enumerates every reachable SHA1 before
  any encoding starts, so abbreviated prefixes get the same verdict
  regardless of how far the translation has progressed. Ambiguous
  prefixes are left unrewritten and reported (warning on stderr +
  `--json`'s `ambiguousMessageRefs`); look them up in the mapping file.
- **Cross-branch references resolve.** Each in-scope SHA1 mentioned in
  a message is added as a dependency edge in the translation DFS, so
  the referenced commit is translated before the referencing commit is
  encoded. A cherry-pick from a sibling branch resolves just as
  reliably as a revert of an ancestor.

False positives are essentially impossible: a run is substituted only
if its prefix uniquely matches a commit or tag in scope. Blob and tree
hashes are excluded from the match set. Disable with
`--no-rewrite-messages` if you prefer untouched messages.

### Origin notes ref (default on)

`refs/notes/sha1-origin` holds, for each translated commit, the
pre-conversion SHA1 keyed by the new SHA256:

```bash
git -C /path/to/out.git notes --ref=sha1-origin show <sha256>
# prints the original SHA1

git -C /path/to/out.git log --notes=sha1-origin
# shows the original SHA1 below each commit's body
```

Notes attach meaningfully only to commits; blobs, trees, and tags are
not represented. Disable with `--no-origin-notes`.

### Sidecar mapping file (opt in via `--write-mapping`)

`--write-mapping <path>` emits a TSV with one line per translated
object, sorted by SHA1:

```
# sha1   sha256
00027b675386b21c4ca05316145671fb7034d251   d80415fa21bebb...
000bb155604d06f1c48fc7feb4b025d991ef3366   a23cf98db5abfa...
...
```

Useful for bulk rewriting external systems: feed the file to a script
that walks Jira tickets, PR bodies, deploy manifests, or any other
system that holds frozen SHA1 references.

### Branch-tip attestation tags (opt in via `--sign-mode tips`)

`--sign-mode` defaults to `none` (sign nothing). `--sign-mode tips`
shells out to `git tag -s converted/<branch> <tip>` for every
converted branch after the conversion completes. Each resulting
signed annotated tag is a cryptographic attestation by the converter
that the entire reachable history of that branch — every parent, tree,
and blob — is what the converter saw at conversion time. Anyone can
verify the chain afterwards with `git verify-tag refs/tags/converted/<branch>`.

The mechanism is the standard one: parent hashes are part of each
commit's bytes, so the tip's hash transitively commits to the whole
history. Signing the tip attests every ancestor.

Important nuance: the signature is by the *converter*, not by the
original authors (whose own signatures are necessarily lost — see
"GPG signatures are stripped" under Sharp Edges). The attestation
chain becomes "*X attests this is the conversion they produced*"
rather than "*the original authors wrote this commit*". For internal
mirrors or single-identity repos that's a strict improvement over
unsigned-everywhere; for broad public repos it is weaker than the
pre-conversion chain.

Signing uses the target repo's git signing config (`user.signingkey`,
`gpg.format`) by default — same as `git commit -S` or a normal
`git tag -s`. Override with `--sign-key <id>`, which is passed to
`git tag -s -u <id>`. SSH signing (`gpg.format = ssh`) and OpenPGP
both work because we shell out to `git`.

Requires the `git` binary on `PATH`. Signing failures (no key
configured, gpg/ssh-agent unavailable, etc.) abort the run after the
conversion has already completed — the target repo is left in a
valid converted state, just without the attestation tags. Re-run
`git tag -s converted/<branch> <tip>` manually once the signing
identity is set up.

## Flags

```
--source-url                       source repository URL
--source-token                     source password/token (prefer env)
--source-username                  source basic auth username (default git)
--source-bearer-token              source bearer token
--source-insecure-skip-tls-verify  skip TLS verification (testing only)
--source-follow-info-refs-redirect follow /info/refs cross-host redirects
--target-dir                       SHA256 bare repo directory (must be empty)

--all-refs                         also include refs/* outside heads/tags
                                   (notes, custom namespaces; excludes
                                   pull/merge-request refs by default)
--exclude-ref-prefix               subtract refs by prefix; repeatable
--include-pull-refs                with --all-refs, also convert
                                   refs/pull/*, refs/pull-requests/*,
                                   refs/merge-requests/* (off by default)

--protocol                         protocol mode (auto, v1, v2)
--write-mapping                    write SHA1 → SHA256 TSV to this path
--no-rewrite-messages              skip inline hash rewrites in messages
--no-origin-notes                  skip refs/notes/sha1-origin
--check                            verify the output (config, HEAD, refs, git fsck)
--sign-mode                        signing mode: none (default) or tips
                                   (sign each branch tip as
                                   refs/tags/converted/<branch> via `git tag -s`)
--sign-key                         signing key id passed to `git tag -s -u <key>`
--keep-source-objects              leave the temp SHA1 store on disk
--progress                         live per-phase object counts (TTY only)
--json                             machine-readable output
--verbose, -v                      verbose logging
```

There are no `--branch`, `--tags`, or `--map` flags: scope is fixed to
every branch and every tag on the source.

Environment fallbacks: `GITSYNC_SOURCE_TOKEN`, `GITSYNC_SOURCE_USERNAME`,
`GITSYNC_SOURCE_BEARER_TOKEN`, `GITSYNC_SOURCE_INSECURE_SKIP_TLS_VERIFY`,
`GITSYNC_SOURCE_FOLLOW_INFO_REFS_REDIRECT`, `GITSYNC_PROTOCOL`.

## Sharp Edges

**GPG signatures are stripped.** A signature is bytes signed over the
commit's pre-conversion content (including the SHA1 hashes in `tree`
and `parent` lines). After rewriting, the bytes no longer match the
signature, so verification would always fail; the command drops them
and prints a count. Signed annotated tags lose their signature the
same way. `mergetag` headers on merge commits — which embed a signed
tag with its own signature — are removed entirely, since the embedded
tag references original SHA1s and the signature was computed over
those original bytes.

**Submodule gitlinks must resolve in-repo.** Tree entries with mode
`160000` reference a commit in another repository, but a SHA1 hash
cannot be embedded in a SHA256 tree. The command fails-fast in the
discovery pass — before the target bare repo is initialized — naming
the offending tree, entry, and hash. Convert the submodule repository
first so its commit hashes are available in SHA256.

**Replace refs and source notes refs become detached.**
`refs/replace/<sha1>` encodes a SHA1 in the ref name, so the name
doesn't match under SHA256 and the replacement never triggers.
`refs/notes/*` trees from the source (copied under `--all-refs`)
encode the target object's hash as the entry name, so notes survive
as data but no longer attach to their original commits. Use the
tool's own `refs/notes/sha1-origin` for the inverse lookup.

**Foreign pull/merge-request refs are excluded by default.** Even under
`--all-refs`, the command skips `refs/pull/*` (GitHub/Gitea/Forgejo),
`refs/pull-requests/*` (Bitbucket), and `refs/merge-requests/*` (GitLab).
These server-internal namespaces hold code proposed from forks and other
branches — content foreign to the repository's own history until it is
reviewed and merged. The converted repo is typically mirrored onward with
`git push --mirror`, and a destination forge may not treat those
namespaces as read-only PR refs; it can surface them as ordinary refs and
thereby republish unreviewed code as if it were part of the repository.
The run prints how many such refs it dropped. Pass `--include-pull-refs`
to convert them anyway (e.g. for a faithful archival mirror you control).

## Operational Notes

**One-off, not incremental.** Each run produces a fresh SHA256 repo
from scratch — there is no "fetch the new SHA1 commits and append to
the existing SHA256 repo" mode. Realistic use: convert once, then
make the converted repo the new canonical store. The conversion is
fully deterministic: branch hashes, tag hashes, **and** the
`refs/notes/sha1-origin` ref are all identical across runs against the
same source state. The notes wrapper commit's timestamp is pinned to
the Unix epoch — or to `SOURCE_DATE_EPOCH` when that environment
variable is set — rather than `time.Now()`, so even the notes ref
reproduces byte-for-byte.

**Loose-object storage.** Every translated object is written as a
loose file under `objects/<aa>/<rest>` — no pack file is produced.
Correct, but slow on filesystems that dislike millions of small files.
Run `git -C <target> gc --aggressive` afterwards to pack the converted
repo down to a single packfile.

**Memory linear in reachable object count.** Two `map[Hash]…`
structures stay live for the whole run: `reachable` (SHA1 → object
type, built by discovery) and `mapping` (SHA1 → SHA256, built by
translation). At cobra scale (~5k objects), kilobytes; at Linux kernel
scale (~16M objects), roughly 2 GB peak.

**Discovery adds a ~1.5× decode pass.** Every reachable object is
decoded twice: once in discovery (no encoding) and once in translation
(decode + encode). The cost buys consistent uniqueness verdicts for
message rewriting and submodule fail-fast.

**Abbreviated-prefix lookup is a linear scan.** Each abbreviated SHA1
in a message triggers an O(reachable) scan to check uniqueness. Fine
to ~100k commits; slower past that. A sorted-prefix index would make
it O(log N), an easy optimization if someone hits the wall.

## Verifying the Output

Pass `--check` and the command runs four sanity checks against the
converted repo at the end of the run, printing one line each:

```
verifying output ...
  ✓ config: extensions.objectformat = sha256
  ✓ HEAD: ffe9fff421b77f2dcc049a95b3b8ba7b9da8976dd61bcf35e9fe2d993babc470
  ✓ refs: 37 / 37 resolve to objects
  ✓ git fsck --full: clean
```

The checks are:

1. **config** — `extensions.objectformat = sha256` is present in
   `<target>/config`.
2. **HEAD** — resolves to a non-zero hash and that object exists in
   the store.
3. **refs** — every written ref resolves to an object in the store.
   Side outputs this run created — `refs/notes/sha1-origin` and any
   `--sign-mode tips` attestation tags — are counted separately, so the
   reported total matches `RefsConverted`.
4. **git fsck --full** — the external `git` binary runs a full
   integrity check. Skipped (and reported as such) when `git` isn't
   on `PATH`; the conversion still succeeds.

If any check fails the command exits non-zero. The full per-check
results are also in `--json`'s `checks` array. Without `--check` no
verification runs and the run completes as soon as the conversion
itself finishes.

You can also run the checks by hand on a converted repo, with or
without `--check`:

```bash
git -C /path/to/out.git fsck --full                     # zero errors expected
git -C /path/to/out.git config extensions.objectformat  # prints sha256
git -C /path/to/out.git log --oneline -5                # SHA256 hashes
git -C /path/to/out.git log --notes=sha1-origin -5      # with original SHA1
```

To use the result as a working repo:

```bash
git clone /path/to/out.git /path/to/checkout
```

To serve it from a host that accepts SHA256:

```bash
git -C /path/to/out.git push --mirror <new-remote-url>
```

## Implementation Notes

The pipeline runs in four phases (pack fetch → discovery → target init →
translation), with refs and side outputs written at the end. Submodule
errors surface in discovery, before the target repo is materialized.

Translation is a memoized recursive DFS. Tree, parent, tag-target, and
message-reference edges are all part of the DFS, so the mapping is
populated by the time any object's bytes are encoded. A defensive
`inProgress` set guards against cycles; real Git histories can't form
them (parent/tree/tag-target edges are a DAG, and SHA1 message-
reference cycles are cryptographically infeasible), but a trip into
the guard becomes a hard error rather than a stack overflow.

Translated objects are written with go-git's `SetEncodedObject`. Each
one is built through the target store's `NewEncodedObject`, which binds
it to the store's SHA256 hasher, so both the returned hash and the
on-disk loose path are computed under SHA256. (Earlier revisions wrote
loose objects by hand: `go-git/v6@v6.0.0-alpha.3`'s
`plumbing/format/objfile.Writer` hardcoded SHA1 in its hasher and would
have placed every translated object at a SHA1-derived path. That was
fixed upstream in `v6.0.0-alpha.4`, which derives the hash format from
the store config.) A unit test recomputes `sha256` of every loose
object's decompressed content and compares it against the filename to
guard against a regression.
