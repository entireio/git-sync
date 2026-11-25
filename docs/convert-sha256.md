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
   - **Commits**: `tree` and `parent` hashes translated; the tag
     embedded in a `mergetag` header converted like any tag;
     `gpgsig` and `gpgsig-sha256` headers kept as they are.
   - **Tags**: target hash translated; a body signature over the SHA1
     form moves into a `gpgsig` header, where a SHA256 object keeps it.

   This is the conversion git itself performs for
   `extensions.compatObjectFormat` (`object-file-convert.c`), so every
   converted object converts back to the original SHA1 object, byte for
   byte, and its signatures keep verifying against that SHA1 form.
   A commit header git does not know fails the run, since it might embed
   a hash.
6. Writes refs at the translated tip hashes; points HEAD at the
   source's advertised HEAD when it was converted, else `main`/`master`,
   else the first branch alphabetically (a tags-only conversion leaves
   HEAD at the init default); builds `refs/notes/sha1-origin` (unless
   `--no-origin-notes`); emits the `--write-mapping` TSV (if requested).

## Side Outputs

The conversion deliberately decouples SHA1 from SHA256 — two runs of
this tool against the same source produce SHA256 hashes that share
nothing with the originals. Three on-ramps help bridge the gap.

### Inline message rewriting (opt in via `--rewrite-messages`)

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
hashes are excluded from the match set.

Rewriting is off by default because git's own conversion never touches
messages. A rewritten commit or tag no longer converts back to its SHA1
original, so its signatures stop verifying and a SHA1 repository kept as
a reference cannot be followed through it without
`refs/notes/sha1-origin`. `--no-rewrite-messages` is accepted and does
nothing.

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
original authors. Their own signatures are kept (see "Signatures sign
the SHA1 form" under Sharp Edges), but they sign the SHA1 form of each
object, so the attestation is what vouches for the SHA256 form directly.

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
--rewrite-messages                 rewrite SHA1 hashes in messages to SHA256
                                   (breaks the round trip and signatures of
                                   rewritten objects)
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

**Signatures sign the SHA1 form.** A commit's `gpgsig` header and a
tag's signature are bytes signed over the SHA1 form of the object. They
are kept, in the places a SHA256 object keeps a SHA1 signature, and they
verify against the SHA1 form, which converting back reproduces exactly.
`git verify-commit` in the converted repository looks for a
`gpgsig-sha256` signature over the SHA256 form, which only commits signed
after the cutover carry.

Earlier versions of this command stripped signatures and `mergetag`
headers and rewrote messages by default. Repositories converted that way
do not convert back to their SHA1 originals. `refs/notes/sha1-origin`
still records where each commit came from.

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

To keep the original SHA1 repository following the converted one, see
[Keeping the SHA1 Repository as a Reference](#keeping-the-sha1-repository-as-a-reference).

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
the opt-in message rewriting and submodule fail-fast.

**Abbreviated-prefix lookup is a linear scan.** Each abbreviated SHA1
in a message triggers an O(reachable) scan to check uniqueness. Fine
to ~100k commits; slower past that. A sorted-prefix index would make
it O(log N), an easy optimization if someone hits the wall.

## Keeping the SHA1 Repository as a Reference

After the cutover, the SHA1 repository can stay around as a read-only
reference that follows the SHA256 one. Make the SHA256 repository the one
people push to, protect the SHA1 repository so only the git-sync identity
can write to it, and sync from SHA256 to SHA1 with an ordinary `sync`:

```bash
git-sync sync \
  --branch main --tags \
  https://git.example/new-sha256-repo.git \
  https://github.com/source-org/source-repo.git
```

git-sync sees that the source advertises `object-format=sha256` and the
target SHA1, and translates objects instead of relaying packs. The SHA1
repository ends up with the commits a developer would have made there
directly: same trees, same authors, same dates, same messages. Objects are
converted as git converts them for `extensions.compatObjectFormat`
(`object-file-convert.c`), so each SHA1 object is the one git itself would
compute for the SHA256 object.

**History from before the cutover keeps its original hashes.** A
repository converted with signatures kept and messages untouched converts
back to the original SHA1 objects on its own. One converted by an earlier
version, or with `--rewrite-messages`, does not. git-sync reads
`refs/notes/sha1-origin` from the SHA256 repository and uses the recorded
SHA1 commit for every converted one, so keep that ref when you push the
converted repository to its host. Those commits exist only in the original
repository, so the target must be the repository the SHA256 one was
converted from. Before pushing, git-sync asks the target for the commits the
new history builds on, and stops with an error naming one it lacks.

**git-sync records what it translates.** Each sync updates
`refs/notes/sha256-origin` on the SHA1 repository with the SHA256 commit
behind every commit it wrote, and reads it back on the next run, so a
commit is never translated twice. The branches, tags and notes go in one
atomic push when the SHA1 server supports it, as git's does, so a branch
never lands without its notes. Against a server without atomic pushes, a
branch can land while its notes are refused. The next sync then translates
those commits again, gets the same hashes and writes the missing notes:

```bash
git log --notes=sha256-origin    # in a clone of the SHA1 repository
```

**Tags are immutable.** A tag the SHA1 repository already has is left alone,
whatever it points at, so tags that existed before the cutover keep their
original objects. New tags are translated and created.

**A write to the SHA1 repository blocks the sync.** If a branch on the SHA1
repository points at a commit that is not a translation of any SHA256
commit, or one that is not an ancestor of the new tip, the branch is blocked
and the run fails. Force flags are not supported in this mode.

What translation changes:

- **Signatures are kept.** A commit keeps its `gpgsig` and `gpgsig-sha256`
  headers. A tag's body signature, over the SHA256 form, moves into a
  `gpgsig-sha256` header, and a `gpgsig` header, over the SHA1 form, becomes
  its body signature. `mergetag` headers are converted. A signature over the
  SHA256 form verifies against that form, so `git verify-commit` on the SHA1
  side needs a `gpgsig` signature, which git writes when the commit is made
  in a repository with `extensions.compatObjectFormat = sha1`.
- **Hashes in messages are copied as they are.** A `git revert` made on the
  SHA256 side names the reverted commit by its SHA256 hash, on both sides.
- **Submodule gitlinks fail the sync**, as they fail conversion.
- **`refs/tags/converted/*`** attestation tags from `--sign-mode tips` are
  never synced.

Limits of the current implementation:

- Only SHA256 to SHA1 is supported, in `sync` mode, for branches and tags.
  `replicate`, `bootstrap`, `--prune`, `--all-refs` and the force flags are
  refused.
- Every run fetches the full history of the synced refs into memory, bounded
  by `--materialized-max-objects`, and the full history of
  `refs/notes/sha256-origin` from the target. The push sends only the
  objects the SHA1 repository lacks.

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
