# SHA1 → SHA256 Conversion

`git-sync convert-sha256` is a one-off migration command that fetches a pack
from a SHA1 HTTP source and writes a new SHA256 bare repository on disk.
Every reachable object is re-hashed under SHA256 and tree, commit, and tag
references are rewritten accordingly.

The command is intentionally narrow: it does not push to a remote, it does
not modify the source, and it is meant to be run once per repo. Resulting
SHA256 hashes have no relation to the original SHA1 hashes beyond a
mapping that the command can optionally emit.

## Quick Start

```bash
git-sync convert-sha256 --tags \
  https://github.com/source-org/source-repo.git \
  /path/to/out.git
```

The target directory must not exist or must be empty. The result is a bare
repository with `extensions.objectformat = sha256` and a `refs/notes/sha1-origin`
ref recording each commit's pre-conversion SHA1.

For a private source, pass the token via the environment so it isn't
exposed in `ps`:

```bash
GITSYNC_SOURCE_TOKEN=ghp_xxx git-sync convert-sha256 --tags \
  https://github.com/source-org/private-repo.git \
  /path/to/out.git
```

## What It Does

1. Probes the source via smart HTTP and discovers refs matching the
   requested scope (`--branch`, `--tags`, `--all-refs`, `--map`,
   `--exclude-ref-prefix`).
2. Fetches a single self-contained pack via `upload-pack` and lands it in
   a temporary on-disk SHA1 bare repo. The temp directory is cleaned up
   on exit unless `--keep-source-objects` is passed.
3. Initializes the target as a bare SHA256 repository
   (`git init --object-format=sha256` equivalent).
4. Runs a **discovery pass** that walks every reachable object from
   each desired ref tip and records its SHA1 and object type. This
   gives the rewriter an authoritative "what's in scope" set so
   abbreviated message references can be resolved consistently and
   message-reference edges can be added to the translation graph.
5. Translates every reachable object in topological order via a
   memoized DFS:
   - **Blobs**: re-hashed under SHA256; content unchanged.
   - **Trees**: each entry's hash translated via the in-memory mapping;
     submodule gitlinks left as-is when the referenced commit is in
     this repo, otherwise the run errors.
   - **Commits**: `tree` and `parent` hashes translated; GPG signatures
     dropped; `mergetag` extra headers dropped; in-scope SHA1
     references in the message are translated first (so their SHA256s
     are known) and then substituted into the message.
   - **Tags**: target hash translated; signatures dropped; tag message
     hashes rewritten with the same edge mechanism as commits.
6. Writes refs in the SHA256 target at the translated tip hashes. HEAD
   is repointed at the source's symbolic HEAD when that ref made it
   into the conversion.
7. Optionally writes the SHA1 → SHA256 mapping as a TSV sidecar
   (`--write-mapping <path>`).

The temp SHA1 store is on disk, not in memory, so peak RAM is bounded
by the in-memory mapping plus a small fixed delta-resolution cache.
Large repos still work; expect runtime dominated by the network fetch
and the loose-object write throughput.

## Handling External SHA1 References

A SHA1 → SHA256 cutover is destructive for external systems that
reference commits by hash: PR descriptions, issue trackers, deploy
logs, container labels, doc links, and so on all stop resolving. The
command offers three on-ramps for migrating those out of band.

### 1. Inline message rewriting (default on)

Commit and tag messages are scanned for 7-to-40-character hex runs.
When a run uniquely matches a commit or tag SHA1 in the conversion's
reachable set, it is replaced with the full SHA256 hex. Examples that
get rewritten:

```
Reverts: a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0    →    full SHA256
Cherry-picked from a1b2c3d                            →    full SHA256
```

Two design notes:

- **Uniqueness is decided against the reachable set, not the
  in-flight mapping.** The discovery pass enumerates every reachable
  SHA1 before any encoding starts, so abbreviated prefixes get the
  same verdict regardless of how far the translation has progressed.
  If `a1b2c3d` matches two different commits in scope, it is treated
  as ambiguous and left alone — never rewritten on the basis of which
  one happened to be translated first.
- **Cross-branch references work the same as ancestor references.**
  Each in-scope SHA1 mentioned in a commit's message is added as a
  dependency edge in the translation DFS — that commit is translated
  before the referencing commit is encoded. So a cherry-pick from a
  sibling branch resolves just as reliably as a revert of an
  ancestor. (Cycles in this graph are cryptographically impossible:
  for both A's message to contain B's SHA1 and vice versa, you would
  need to know each hash before computing it.)

False positives are essentially impossible because a run is only
substituted if its prefix uniquely matches a real SHA1 of a commit or
tag in scope. Blob and tree hashes are excluded from the match set
so incidental hex strings that collide with content hashes are not
rewritten. Disable with `--no-rewrite-messages` if you prefer
untouched messages.

### 2. Origin notes ref (default on)

`refs/notes/sha1-origin` is written after translation and holds, for
each translated commit, the pre-conversion SHA1 hex keyed by the new
SHA256 hash. Standard git tooling can read it:

```bash
git -C /path/to/out.git notes --ref=sha1-origin show <sha256>
# prints the original SHA1

git -C /path/to/out.git log --notes=sha1-origin
# shows the original SHA1 below each commit's body
```

Notes attach meaningfully only to commits, so blobs, trees, and tags
are not represented in this ref. Disable with `--no-origin-notes`.

### 3. Sidecar mapping file (opt in)

`--write-mapping <path>` emits a TSV with one line per translated
object, sorted by SHA1:

```
# sha1   sha256
00027b675386b21c4ca05316145671fb7034d251   d80415fa21bebb...
000bb155604d06f1c48fc7feb4b025d991ef3366   a23cf98db5abfa...
...
```

Useful for bulk rewriting external systems: feed the file to a
script that walks Jira tickets, PR bodies, deploy manifests, or any
other system that holds frozen SHA1 references.

## Flags

```
--source-url                       source repository URL
--source-token                     source password/token (prefer env)
--source-username                  source basic auth username (default git)
--source-bearer-token              source bearer token
--source-insecure-skip-tls-verify  skip TLS verification (testing only)
--source-follow-info-refs-redirect follow /info/refs cross-host redirects
--target-dir                       SHA256 bare repo directory (must be empty)

--branch                           comma-separated branch list
--tags                             include annotated and lightweight tags
--all-refs                         include every refs/* on the source
--exclude-ref-prefix               subtract refs by prefix; repeatable
--map                              ref mapping in src:dst form; repeatable

--protocol                         protocol mode (auto, v1, v2)
--write-mapping                    write SHA1 → SHA256 TSV to this path
--no-rewrite-messages              skip inline hash rewrites in messages
--no-origin-notes                  skip refs/notes/sha1-origin
--keep-source-objects              leave the temp SHA1 store on disk
--json                             machine-readable output
--verbose, -v                      verbose logging
```

Environment fallbacks: `GITSYNC_SOURCE_TOKEN`, `GITSYNC_SOURCE_USERNAME`,
`GITSYNC_SOURCE_BEARER_TOKEN`, `GITSYNC_SOURCE_INSECURE_SKIP_TLS_VERIFY`,
`GITSYNC_SOURCE_FOLLOW_INFO_REFS_REDIRECT`, `GITSYNC_PROTOCOL`.

## Sharp Edges

**GPG signatures are stripped.** A signature is bytes signed over the
commit's pre-conversion content (including the SHA1 `tree` and
`parent` lines). After rewriting, the bytes no longer match the
signature, so verification would always fail. Rather than persist
invalid signatures, the command drops them and prints a warning count.
This matches upstream `git`'s own SHA256 conversion behavior. Tags
with embedded signatures and `mergetag` extra headers are handled the
same way.

**Submodule gitlinks must resolve in-repo.** Tree entries with mode
`160000` reference a commit in another repository, but a SHA1 hash
cannot be embedded in a SHA256 tree. The command translates the
pointer if the referenced commit happens to live in the same store
(rare; sometimes seen in vendored modules), and otherwise exits with
an error naming the offending tree, entry, and hash. Scope around
those refs with `--exclude-ref-prefix` or `--branch`, or convert the
submodule repository first.

**External SHA1 references break silently.** See the section above for
mitigations. References inside the repo (commit and tag messages) are
rewritten when they uniquely identify a commit or tag in scope.
Anything outside the repo — PR descriptions, issue trackers, deploy
manifests, container labels — is not the converter's job; use the
mapping file to drive those rewrites.

**Replace refs and notes refs become detached.** `refs/replace/<sha1>`
encodes a SHA1 in the ref name itself, so the name doesn't match
under SHA256 and the replacement never triggers. `refs/notes/*` paths
encode the target object's hash as a tree path, so existing notes
copied under `--all-refs` survive as data but no longer attach to
their original commits. Neither is a correctness issue, just lost
behavior.

**HEAD can dangle.** When the source's symbolic HEAD branch is not in
the desired ref set, the target's HEAD is left at `refs/heads/master`
(go-git's PlainInit default) and resolves to nothing. Either include
the HEAD branch in scope or set it manually after conversion with
`git -C <target> symbolic-ref HEAD refs/heads/<branch>`.

**Storage is all loose objects.** The command writes one file per
object. Correct, but on filesystems that dislike millions of small
files this is slow. Run `git -C <target> gc --aggressive` afterwards
to pack the converted repo down to a single packfile.

## Verifying the Output

Standard git tooling works against the converted repo without
additional flags — the `extensions.objectformat` setting in the local
config is enough for git to switch hashing:

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

The translator works in four phases:

1. **Pack fetch.** A single self-contained pack is streamed into a
   filesystem-backed SHA1 storer via go-git's pack parser, so deltas
   are resolved up front and the SHA1 source is randomly addressable
   for the rest of the run.

2. **Discovery.** A non-encoding DFS walks every object reachable
   from each desired ref tip via tree entries, commit
   tree+parent links, and tag targets. Each visited SHA1 is recorded
   in a `reachable map[Hash]ObjectType`. This set is the authoritative
   "what is in scope" answer used by both submodule resolution and
   message-reference rewriting — uniqueness of abbreviated SHA1
   prefixes is decided against this set once, never against the
   in-flight mapping.

3. **Translation.** Memoized recursive DFS from each desired ref tip.
   Blobs are copied as-is and re-hashed; trees, commits, and tags are
   decoded, their embedded hashes rewritten via the SHA1 → SHA256
   mapping, signatures stripped, and messages rewritten. The DFS
   recursion includes message-reference edges: for each commit or
   tag whose message mentions a SHA1 of a commit or tag in the
   reachable set, that referenced object is translated first. This
   guarantees the mapping is populated before the substitution
   happens, so cross-branch references resolve as reliably as
   ancestor references. Each translated object is written as a loose
   object under `objects/<aa>/<rest>` in the target.

4. **Refs and side outputs.** Refs and HEAD are written at the
   translated tip hashes; the origin notes commit (if enabled) is
   built and stored under `refs/notes/sha1-origin`; the mapping file
   (if requested) is written.

A defensive `inProgress` set guards against cycles during phase 3.
Real Git histories cannot form cycles (parent, tree, and tag-target
edges are a DAG by construction, and SHA1 message-reference cycles
are cryptographically infeasible), so a trip into this branch is a
hard error rather than a silent skip.

Note: loose object writing is done by hand rather than via go-git's
`SetEncodedObject`. The underlying `plumbing/format/objfile.Writer`
in `go-git/v6@v6.0.0-alpha.3` hardcodes SHA1 in its hasher, which
would put every translated object at a SHA1-derived path even though
the content references SHA256. This is verified by a unit test that
recomputes `sha256` of every loose object's decompressed content and
compares against the filename.
