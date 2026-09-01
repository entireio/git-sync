# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/),
and this project adheres to [Semantic Versioning](https://semver.org/).

## [Unreleased]

### Fixed

- **An indivisible one-commit bootstrap checkpoint is now tested against the target's announced pack limit before the run gives up.** The batching budget is intentionally smaller than that limit, so treating a pack that crossed the budget as permanently too large made every retry fail locally even when the target would have accepted it. Once subdivision bottoms out at one commit, git-sync now attempts that pack and only reports a permanent failure when the target itself refuses it. Deadline and cancellation errors retain their original classification instead of being converted into a size verdict ([#118](https://github.com/entireio/git-sync/pull/118))

- **A batched bootstrap no longer discards its resume position when the target refuses the branch create.** The cutover push carries two commands — advance the `refs/gitsync/bootstrap/heads/<branch>` temp ref to the final checkpoint, and create the real branch there — and the temp ref was deleted on the strength of that push returning no error. Under `--best-effort` (implied by `--all-refs`) a per-ref `ng` reaches a callback and the push still returns `nil`, so a target that refuses the create — a protected branch, a pre-receive policy — ended the run with neither the branch nor the marker and reported success. With nothing on the target referencing the objects already delivered, the next run had no fetch have to negotiate against and re-transferred the entire history, on precisely the large repositories batching exists for. Whether the branch landed is now settled against the target's own refs, not inferred from the push, and the marker survives anything short of the branch being present at the hash the run pushed ([#117](https://github.com/entireio/git-sync/pull/117))

### Changed

- **Embedding callers now receive the populated result alongside errors returned after planning has begun.** `Plan`, `Sync`, and `Replicate`, plus their unstable counterparts and `Bootstrap`, preserve the selected operation and transfer modes, relay fallback reason, attempted ref plan, and completed batch progress when execution fails. Callers can therefore distinguish a failed relay from a failed or partially completed bootstrap without reconstructing the route from the error text. Outcome counts remain intentionally unpopulated on this path and must not be used to infer how much landed ([#118](https://github.com/entireio/git-sync/pull/118))

- **A target that refuses the batched bootstrap temp ref now fails the run, including under `--best-effort`.** Every other per-ref refusal stays a warning there, but this one is the batching state machine: the run previously advanced its checkpoint position against a ref the target had not moved, then failed later and more confusingly, or deleted a marker at a hash the target never accepted. A run against a target that blocks writes to `refs/gitsync/*` used to exit 0 with warnings and now exits non-zero — worth knowing if you alert on exit codes ([#117](https://github.com/entireio/git-sync/pull/117))

### Added

- **`ErrCheckpointExceedsTargetLimit` identifies a permanent, target-verified bootstrap size failure.** `Sync`, `Replicate`, and `Bootstrap` wrap this sentinel only after the target refuses a one-commit checkpoint that cannot be subdivided further, so embedders can terminate futile redelivery with `errors.Is` while continuing to retry transport failures and deadline expiry ([#118](https://github.com/entireio/git-sync/pull/118))

## [0.9.0] - 2026-08-28

### Security

- Bumped `golang.org/x/crypto` to `v0.54.0`, closing 13 Dependabot alerts (7 critical, 3 high, 3 medium), all of them in its SSH implementation. Raised as an indirect dependency via minimal version selection, since go-git still asks for `v0.51.0`; lifts `golang.org/x/net` to `v0.56.0` and `golang.org/x/sys` to `v0.47.0` ([#104](https://github.com/entireio/git-sync/pull/104))

- Bumped go-git to `v6.0.0-alpha.5`, clearing two advisories `govulncheck` reported as reachable from git-sync's own call graph: **GO-2026-6214**, path traversal via crafted reference names, reached where `convert-sha256` writes advertised refs to disk; and **GO-2026-6213**, worktree operations following symlinks. Pulls `go-billy` to `v6.0.0-alpha.2` transitively ([#105](https://github.com/entireio/git-sync/pull/105), [#106](https://github.com/entireio/git-sync/pull/106))

- Bumped the Go toolchain to `1.26.6` (go.mod `toolchain` and the mise pin), clearing ten standard-library advisories reachable from the HTTP transport and the `convert-sha256` filesystem paths — among them quadratic complexity in `net/url` `resolvePath`, an `os` root escape via symlink plus trailing slash, and an HTTP/2 infinite loop on a bad `SETTINGS_MAX_FRAME_SIZE`. Released binaries are built from the go.mod toolchain, so they carried these until now; `govulncheck ./...` goes from 12 called vulnerabilities to 0 ([#106](https://github.com/entireio/git-sync/pull/106))

- **Explicitly supplied credentials are now bound to the host you named.** Under `--source-follow-info-refs-redirect` / `--target-follow-info-refs-redirect`, git-sync addresses the host `/info/refs` redirected to directly for the follow-up RPCs, and re-attached the configured token to those requests after Go's `http.Client` had deliberately stripped the `Authorization` header on the cross-host hop — so any redirect an attacker could cause (an open redirect on the real host, a hostile mirror, a man-in-the-middle on a plain-`http` source) collected a token that on the target side typically carries write access. A token now goes only to the host named on the command line and its subdomains, on the same port and without a scheme downgrade, and anything else falls through to the credential helper with a warning naming the withheld host. Credentials resolved *from* the helper are unaffected — those were always looked up keyed on the host being challenged ([#108](https://github.com/entireio/git-sync/pull/108))

- **Ref names from a remote are validated before git-sync acts on them.** Unchecked names reached two places where a malformed one matters: `convert-sha256` writes them to disk, where `refs/heads/../../config` escapes `refs/heads` (go-billy clamps the traversal at the repository root, so the reachable damage is overwriting `config`, `HEAD`, or `packed-refs`); and ref-update commands embed them in the receive-pack request, where a NUL lets the source inject a capability list into the push git-sync sends to the *target*. Advertised names now go through `plumbing.ReferenceName.Validate` — git's own `check_refname_format`, so nothing git accepts is affected — at both decode boundaries, on the source and target side. A failing name is skipped with a quoted warning rather than failing the run; names supplied through `--map` are rejected outright at startup ([#109](https://github.com/entireio/git-sync/pull/109))

- **A packfile object header can no longer request an unbounded allocation.** `ExtractCommitParents` sized its per-object buffer from the size the pack header *declares*, which go-git validates only for varint overflow — so the value is remote-controlled up to `int64`, and a 48-byte pack declaring a 128 TiB object produced `fatal error: out of memory`, a Go runtime fatal an embedding process cannot contain with `recover()`. Declared sizes above 64 MiB are now rejected (this path reads `tree:0`-filtered packs, whose objects are commits and tags measured in kilobytes) and the initial buffer is capped at 64 KiB, so the allocation tracks bytes actually received. Reachable from ordinary planning, via `FetchCommitParents` during ancestry checks ([#107](https://github.com/entireio/git-sync/pull/107))

- **SSH repository paths are now shell-quoted in full.** `shellQuotePath` exempted the segment before the first slash when the path began with `~`, leaving it for the remote login shell to expand — and a SCP-style URL puts attacker-influenceable text in exactly that position: `git@host:~a;id/repo.git` produced the remote command `git-upload-pack ~a;id/'repo.git'`, executing `id` on the remote host as the authenticated SSH user. This matters wherever only part of the URL is caller-controlled, such as a repository path templated into a fixed host or one arriving from a webhook. Quoting the whole path costs nothing, because `git-upload-pack` resolves it through `enter_repo()`, which interpolates a leading `~` itself ([#107](https://github.com/entireio/git-sync/pull/107))

- **Credentials embedded in a remote URL are no longer echoed.** `probe` and `fetch` carried `Source.URL`/`Target.URL` verbatim into their results, which are printed to stdout and serialized into `--json` — so `https://user:token@host/repo.git`, the standard CI form, put the token in output that automation collects; the URL-parse failure path leaked it too, since `url.Parse` embeds the raw URL in its error. All of these now redact the whole userinfo component via the new `internal/redact` package, which also replaces `url.URL.Redacted()` in HTTP error messages: `Redacted()` masks only the password, leaving a token in the *username* position — how GitHub App and PAT URLs are normally written — in the clear ([#107](https://github.com/entireio/git-sync/pull/107))

- **The materialized object limit is enforced while the pack streams, not after.** `FetchToStore` filled the in-memory store with no cap, and `--materialized-max-objects` (default 500,000) was checked against the object closure only once the fetch had finished — by which point the objects were resident and the process may already have died, so the guard reported an overrun it exists to prevent. The store now enforces the count as objects are decoded, failing the write that would exceed the limit, with an `ErrObjectLimit` sentinel and an `ObjectLimitError` carrying the limit. The cap applies per fetch rather than cumulatively, because a store outlives one fetch and the materialized tag refill legitimately resends objects already present, so worst-case residency is the limit times the number of fetches — two on the sync path. The unstable `Fetch` path now actually threads `MaterializedMaxObjects` through, having previously accepted and dropped it ([#110](https://github.com/entireio/git-sync/pull/110))

- **Advertisement reads are bounded on every transport.** The HTTP path capped `/info/refs` at 64 MiB, but the SSH transport read the advertisement with a bare `io.ReadAll` and the remote-helper transport accumulated pkt-lines until a flush that a remote need never send — either grows until the process dies. All three now share one `MaxAdvertisementBytes` ceiling, and hitting it tears the producer down rather than leaving a subprocess blocked on a full pipe and hanging `Wait` forever ([#107](https://github.com/entireio/git-sync/pull/107))

- **The commit-graph pack spill to disk is bounded.** `ExtractCommitParents` copies a non-seekable pack into a temp file to give the parser a seekable source, with no limit on the copy, so a source could fill the disk during what is only a planning round trip. Capped at 4 GiB ([#107](https://github.com/entireio/git-sync/pull/107))

- **Server-authored text is stripped of terminal control characters.** Sideband progress, up to 64 KiB of HTTP error body, receive-pack `ng` rejection reasons, diagnostic response headers, and ssh's relayed `remote:` output all reached terminals, log files and `--json` results unfiltered, so a hostile remote could redraw the line its message was printed on — making a rejected push read as a successful one — or smuggle control characters into whatever ingests the JSON. All five paths now go through `internal/sanitize`, which drops everything below `0x20` plus DEL. Carriage return survives only in streamed progress, where git's in-place updates depend on it; one-shot messages lose it, because a bare CR rewrites the line on its own (`rejected\rok refs/heads/main` reads as a success with no escape sequence involved). Rejection classification still runs on the raw status, so filtering cannot change whether a rejection is treated as a concurrent move ([#111](https://github.com/entireio/git-sync/pull/111))

- **Whether TLS verification is disabled is now derived from the transport.** `HTTPConn.InsecureSkipTLSVerify` was a field callers had to set to match the client they passed in, and the cross-host credential guard depends on it — so a caller who disabled verification on their transport but forgot the field silently lost that protection, which is the wrong direction for a security check to fail. The transport is now inspected directly, following `Unwrap` through wrappers so an instrumentation layer cannot hide the setting; the explicit field still forces the guard on for transports that cannot be inspected ([#111](https://github.com/entireio/git-sync/pull/111))

- **Release artifacts now carry signed build provenance.** `checksums.txt` was the only integrity signal and was itself unsigned, so someone downloading a Linux archive plus its checksum had no way to establish that either came from this repository — macOS binaries were signed and notarized, but nothing else was. The release workflow now attests the archives and the checksum file via `actions/attest-build-provenance`, verifiable with `gh attestation verify <file> --repo entireio/git-sync`. README documents the check ([#112](https://github.com/entireio/git-sync/pull/112))

- Added `permissions: contents: read` to the `Tests` and `License Check` workflows, which previously inherited the repository default while running PR-controlled code. `lint.yml` already declared it. Defence in depth — GitHub already restricts the token for fork PRs — but same-repo branches are not restricted ([#107](https://github.com/entireio/git-sync/pull/107))

### Added

- **`SyncPolicy.AllowEmptySource` — an empty source can now be an outcome instead of an error, when a source of truth says so.** Replicate previously failed any run whose planning produced no desired refs, with one message (`no source refs matched`) covering unrelated conditions: the source has no refs, the source has refs the requested scope excluded, and the source has refs this reader was never shown. A caller could not tell them apart, and the first is not always a failure — a mirror of a repository that has never been pushed to is trivially up to date. With the policy set, Replicate reports them separately: `ErrNoRefsSelected` when the source does advertise refs, `ErrSourceEmptyUnverified` / `ErrTargetEmptyUnverified` when either side's emptiness could not be established, `ErrSourceEmptyTargetPopulated` when the source is empty while the target still holds refs (a real divergence — refused rather than converged, since converging means deleting them), and a zero-plan success with `ExecutionSummary.Converged` when source and target are both empty and therefore already agree.

  **git-sync does not decide that a repository is empty, and will not guess.** It cannot: ref hiding is designed to be invisible to the client, so a hidden ref and an absent one are the same observation. An unborn HEAD does not close the gap either — git emits that line for any dangling HEAD, so a repository holding `refs/heads/other` with HEAD pointed at a never-created `refs/heads/main` reports unborn, and hiding that branch reduces its entire advertisement to the unborn line alone (verified against git 2.53). The assertion is therefore an input: `SyncPolicy.SourceAssertedEmpty`, which the caller supplies from a repository-state query that sees past hiding.

  The target needs the same treatment, via `SourceAssertedEmpty`'s counterpart `TargetAssertedEmpty`, and for a sharper reason: `receive.hideRefs` omits matching refs from receive-pack's advertisement, so a populated target can advertise nothing but the bare `capabilities^{}` sentinel — and because `receive.hideRefs` and `uploadpack.hideRefs` are separate settings, a ref hidden from the push side is still served to fetchers, so a target wrongly judged empty is one whose readers see refs the source does not have.

  What git-sync contributes is a consistency check on those claims, and it only ever refuses. Before reporting convergence it independently requires an empty advertisement on each side, an unborn HEAD on the source (now requested via protocol v2's `ls-refs=unborn` where the server advertises it, and surfaced as `RefService.HeadUnborn`), no advertised ref name dropped as invalid on either side, and no visible target ref within the request's scope (an excluded namespace the run would neither push nor prune is not divergence). None of those can promote an absent assertion into a success, so a caller cannot get a false converge out of a compliant server, and a caller that supplies no assertion gets `ErrSourceEmptyUnverified` or `ErrTargetEmptyUnverified` no matter what the wire says. The unborn line's `symref-target` is deliberately not surfaced as `SourceHEAD`, which consumers read as a branch that exists.

  Off by default: without the opt-in an empty source still fails with the historical message, and the new sentinels deliberately do not carry that text so a caller still matching on it cannot mistake a divergence for the old benign no-op. The policy is replicate-only and requires `RefScope.AllRefs` — under a narrower scope the source ref listing is itself narrowed, so an empty result says nothing about the repository as a whole — and both requirements are now rejected at the request edge rather than accepted and silently discarded. Convergence also requires protocol v2 on the source leg, since the unborn cross-check has no v1 equivalent: `ProtocolV1` is rejected at the request edge, and an `auto` source that falls back to v1 mid-run reports that the protocol cannot carry the signal rather than implying the server withheld refs.

  One thing does change for every v2 caller, opt-in or not: `unborn` is appended to each `ls-refs` request whose server advertises support for it (see `docs/protocol.md`). It adds no round trip and a source with commits answers exactly as before, but the request bytes differ, so a test asserting on the exact ls-refs body will need updating. Only the *reader* of the resulting flag is gated on the policy ([#114](https://github.com/entireio/git-sync/pull/114))

- A `Vulnerability Scan` workflow running `govulncheck ./...` on pull requests, pushes to main, and a weekly schedule. The existing lint suite cannot see this class of issue, and the weekly run matters because advisories are published against versions already in go.mod — without it, a newly disclosed vulnerability goes unreported until someone happens to open a PR ([#106](https://github.com/entireio/git-sync/pull/106))

## [0.8.0] - 2026-07-09

### Added

- `gitsync.SetIdentity(service, version)` — lets an embedding service name itself in every request git-sync makes. The HTTP User-Agent and the git-protocol `agent=` capability become `<service>/<version> git-sync/<git-sync-version> go-git/<go-git-version>` (non-git provider requests carry the same string without the go-git token). Previously an embedder had no way to identify itself: the advertised version lives in an internal package, and the old doc comment claiming "SDK consumers may overwrite it" was unimplementable from outside the module.

- `RefScope.ExcludeRefs` — exact ref-name exclusion, alongside the existing prefix-based `ExcludeRefPrefixes`. An excluded exact name is not pulled, pushed, or pruned, but — unlike a prefix — its children are unaffected, so a caller can reserve a directory-anchor ref like `refs/heads/entire` while still mirroring `refs/heads/entire/foo`. Threaded through `RefScope` → planner `PlanConfig`; `IsRefExcluded` now takes both prefix and exact lists.

### Changed

- The default advertised git-sync version is now resolved from the binary's embedded build info instead of the hardcoded `"dev"`: embedders automatically advertise the git-sync module version they built against, and a `go install ...@version` CLI build advertises that version. `"dev"` remains only for builds with no usable version (e.g. a plain `go build` of this repo). The goreleaser-stamped CLI version still takes precedence when present.

### Removed

- The built-in Entire DB credential store integration (`hosts.json` active-user lookup, the file/keyring token store, and OAuth refresh-token handling). `auth.Resolve` now resolves only explicit token/bearer credentials; everything else defers to the git credential helper on a 401, exactly as for any other remote. The Entire mirroring pipeline and the `git-remote-entire` helper already supply credentials directly (installation / repo-scoped tokens at the transport layer), so nothing produced the `hosts.json`/token-store layout this code read. This drops the `github.com/zalando/go-keyring` dependency and, with the file token store gone, the package now compiles on Windows without a `flock` shim.

### Fixed

- Concurrent **create** races on the target are now classified as `ErrTargetRefMoved`, matching the existing concurrent-update handling. entire-server rejects a create command (old = zero hash) for a ref that already exists with `already exists`; git-sync only plans a create for a ref it found absent at plan time, so that rejection is an unambiguous benign race — a second sync of the same repo created the ref first — exactly like the update-side `remote ref has changed`. Previously only the update reason was in `concurrentMoveMarkers`, so a create race fell through as a generic push failure and `errors.Is(err, ErrTargetRefMoved)` returned false; embedders that key redelivery/alerting off the sentinel (e.g. mirror-pipeline's worker) misclassified it as a hard sync failure. Both the create and update CAS rejections now satisfy `errors.Is(err, ErrTargetRefMoved)`.

## [0.7.0] - 2026-06-16

### Added

- Typed push-rejection errors on the public API. `Sync`/`Replicate` now report a `*RefRejectedError` (carrying the rejected `Ref` and the raw server `Reason`) for per-ref receive-pack `ng` statuses, reachable with `errors.As`. Rejections that are unambiguous concurrent target-ref moves — entire-server's compare-and-swap rejection (`remote ref has changed`) and git's `--force-with-lease` lease miss (`stale info`) — additionally satisfy `errors.Is(err, ErrTargetRefMoved)`. This lets embedders distinguish a benign racing concurrent push (retryable) from a genuine push failure without substring-matching the free-form error message. Ambiguous markers (`non-fast-forward` / `fetch first`) are deliberately excluded from the move classification so a real "needs `--force`" rejection is not masked. The `ForceWithLease` lease-failure escalation (raised even under `BestEffort`) also satisfies `errors.Is(err, ErrTargetRefMoved)`, though it is not itself a `*RefRejectedError` — prefer `errors.Is` over `errors.As` when you only need the cause. The error message and the underlying value-typed `packp.CommandStatusErr` are preserved unchanged (reach it with a value `errors.As` target), so existing checks keep working ([#71](https://github.com/entireio/git-sync/pull/71))

### Fixed

- Concurrent target-ref rejections are now actually classified — `errors.Is(err, ErrTargetRefMoved)` and `errors.As(err, *RefRejectedError)` match on the real push path. go-git returns `packp.CommandStatusErr` **by value** from `ReportStatus.Error()`, but `asRefRejectedError` / `annotateLeaseFailure` used a `*packp.CommandStatusErr` (pointer) `errors.As` target, which never matches a value in the chain — so every live receive-pack `ng` status passed through unclassified and `ErrTargetRefMoved` was never reported. Both now use a value target, and a regression test drives a real `ReportStatus.Error()` end to end so a pointer-vs-value relapse fails CI. (Bug in the typed-rejection feature above — never shipped in a tagged release.) ([#73](https://github.com/entireio/git-sync/pull/73))
- Batched bootstrap no longer dies when finalizing a subsumed branch against a receive-pack that requires a pack for every non-delete command. The pack-less ref-create sent only command pkt-lines, which strict servers rejected with `unpack error: ... read packfile header: EOF` (observed mid-run mirroring to entiredb prod, leaving the target half-populated); git-sync now sends a valid empty pack with such creates ([#74](https://github.com/entireio/git-sync/pull/74))
- Large or slow bootstrap relays that outlast the target's `git-receive-pack` deadline (GitHub HTTP 408, or gateway 504) now fall back to batched bootstrap with checkpoint subdivision instead of hard-failing. Previously only 413 body-limit rejections triggered batching, so relaying a large repo over a slow source link — where the upstream read rate throttles the downstream write past GitHub's receive-pack window — failed with a bare `http 408` and no remediation. Timeouts are classified distinctly from size rejections, the auto-batch notice names the cause, and a one-shot failure with no batched fallback (source lacking protocol-v2 fetch-filter support) now carries actionable guidance ([#75](https://github.com/entireio/git-sync/pull/75))

## [0.6.0] - 2026-06-03

### Added

- `git-sync convert-sha256`: one-off conversion that fetches a pack over smart HTTP from a SHA1 source, walks every reachable object via a two-pass topological DFS, and writes a fresh SHA256 bare repository with every tree/commit/tag reference re-encoded — including abbreviated SHA1 prefixes in commit messages. All branches and tags are always converted to avoid stranding cross-branch references; sharp edges and operational characteristics are documented in `docs/convert-sha256.md` ([#66](https://github.com/entireio/git-sync/pull/66))

### Changed

- HTTP auth now matches git's own flow: try anonymous first and only consult the credential helper after a 401, instead of proactively running `git credential fill` for every endpoint. This stops git-sync from dropping into an interactive `Username:`/`Password:` prompt on unauthenticated hosts and from leaking tokens to public repos. Expired credentials (401, or 403 from token services like Cloudflare) trigger a helper `reject` so the next run starts clean; the helper runs with `GIT_TERMINAL_PROMPT=0` to fail fast rather than block on a tty ([#65](https://github.com/entireio/git-sync/pull/65))
- Outbound requests now identify git-sync in the User-Agent instead of advertising only go-git's default. Git wire-protocol traffic (smart-HTTP info-refs, upload-pack/receive-pack, and the protocol v2 `agent=` capability) sends `git-sync/<version> go-git/<v>` to preserve go-git attribution; non-git HTTP (the GitHub repo metadata call during bootstrap) sends just `git-sync/<version>`. A new `internal/useragent` package centralises the format, wired from `versioninfo.Version` at startup so `--version` and the User-Agent agree, and overridable by SDK consumers ([#69](https://github.com/entireio/git-sync/pull/69))
- Bootstrap planning streams the commit-graph fetch instead of materializing the full commit set: a new `ExtractCommitParents` path parses the `tree:0` pack incrementally, extracting only `(commit -> parent hashes)` tuples with a bounded LRU for delta resolution. On `torvalds/linux` this cut peak Go heap from 5.42 GiB to 1.47 GiB (-73%), peak RSS from 5.69 GiB to 1.63 GiB (-71%), and wall time from 32m to 19m (-40%) ([#61](https://github.com/entireio/git-sync/pull/61))

### Fixed

- Materialized push against CDN-fronted HTTP targets (e.g. Cloudflare) no longer fails mid-upload with `use of closed network connection`. Two independent causes: a stale keep-alive pool entry expiring on the CDN edge during the gap between info-refs and receive-pack (fixed by disabling keep-alives on the default transport), and a mid-stream stall while go-git ran delta selection synchronously inside `Encode()` (fixed via `packfile.WithObjectSelector` to run selection up front and stream the write phase through `io.Pipe`). Adds `GITSYNC_HTTP_TRACE=1` for per-request lifecycle tracing with redacted auth, plus in-place verbose progress for the materialized encode/write phases ([#64](https://github.com/entireio/git-sync/pull/64))

### Housekeeping

- Bump go-git to v6.0.0-alpha.4 ([#60](https://github.com/entireio/git-sync/pull/60))

## [0.5.0] - 2026-05-18

### Added

- SSH transport: `ssh://`, SCP-style `git@host:path.git`, and `git+ssh://` remotes via the local `ssh` binary, with one process per logical RPC so v2 and batched flows work correctly. SSH config-driven user/key behavior is honored, and a clear error is raised when `ssh` is not on `PATH` ([#54](https://github.com/entireio/git-sync/pull/54), [#56](https://github.com/entireio/git-sync/pull/56))
- `--all-refs` for mirroring arbitrary `refs/*` namespaces (notes, pulls, custom) beyond `refs/heads/*` and `refs/tags/*`. For `sync` and `bootstrap` it bundles a best-effort failure mode that downgrades per-ref `receive-pack` rejections to warnings (surfaced via `Result.Warned` and JSON `warned`), so mirroring into hosts with hidden refs like GitHub `refs/pull/*` works. `replicate` keeps strict semantics. Library exposes `RefScope.AllRefs`, `SyncPolicy.BestEffort`, `RefKindOther`, and `ActionWarn` ([#44](https://github.com/entireio/git-sync/pull/44))
- Bootstrap pushes the source `HEAD`'s branch first, so hosts that pick the default branch from the first push on a fresh repo (GitHub, GitLab) end up with the right default automatically. The source `HEAD` symref is also surfaced on `Result` and `ProbeResult` (`execution.sourceHead` / `sourceHead` in JSON) ([#51](https://github.com/entireio/git-sync/pull/51))

### Changed

- `--force` is replaced by two explicit flags: `--force-with-lease` (previous lease-protected behavior) and `--force-blind` (zero expected-old, overwrite regardless — matches `git push --force`). The flags are mutually exclusive; legacy `--force` errors out with a migration hint. `bootstrap` and `replicate` continue to reject force flags entirely. `SyncPolicy.Force` splits into `ForceWithLease` and `ForceBlind`. Lease-failure `ng` responses from `receive-pack` are annotated with a rerun-or-`--force-blind` hint ([#53](https://github.com/entireio/git-sync/pull/53))
- `replicate` failure messages no longer suggest "use sync instead" for errors that aren't relay-capability problems (network, cancellation, etc.) ([#52](https://github.com/entireio/git-sync/pull/52))

### Fixed

- v1 target pushes against repos with annotated tags no longer fail with `HTTP 400 invalid reference name: refs/tags/<X>^{}`. `AdvRefsToSlice` now drops peeled `^{}` entries that go-git v6 alpha.3 preserves inline; affected `replicate` always and `sync --prune` ([#57](https://github.com/entireio/git-sync/pull/57))

### Housekeeping

- Bump go-git to v6.0.0-alpha.3 ([#49](https://github.com/entireio/git-sync/pull/49), [#55](https://github.com/entireio/git-sync/pull/55))

## [0.4.3] - 2026-05-07

### Added

- `--bootstrap-strategy=topo` for merge-heavy repos: walks every reachable commit in deterministic topological order so batched bootstrap can place sub-pack boundaries inside side-branch ancestry ([#41](https://github.com/entireio/git-sync/pull/41))
- `--progress` for live per-side throughput across `sync`, `replicate`, `bootstrap`, and `fetch`, with rolling-window rate, hostname-aware labels, inline pack subdivision, and an end-of-run summary under `--stats` ([#37](https://github.com/entireio/git-sync/pull/37))
- Per-subcommand `--help` after the CLI moved to cobra; bare `git-sync` lists subcommands on stdout instead of printing the full usage block as an error ([#35](https://github.com/entireio/git-sync/pull/35))
- README cover image and embedded demo videos for `git-sync plan` and `git-sync sync` ([#30](https://github.com/entireio/git-sync/pull/30), [#31](https://github.com/entireio/git-sync/pull/31))

### Changed

- Batched bootstrap stream-parses the pack and aborts doomed pushes ~5% in instead of waiting for the body-limit rejection; subdivision converges in 1–2 rounds instead of 6+ on blob-heavy repos ([#40](https://github.com/entireio/git-sync/pull/40))
- Post-rejection subdivision sizes splits from observed pack bytes (4× when the server cut us off mid-stream, 2× otherwise) and ratchets the bytes-per-object estimate up after each 413 ([#38](https://github.com/entireio/git-sync/pull/38))
- Batched bootstrap recombines upcoming checkpoints when consecutive packs underuse the target limit, recovering pack granularity after heavy regions; already-pushed checkpoints are also passed as fetch `have`s on later rounds ([#42](https://github.com/entireio/git-sync/pull/42))
- Relay-only syncs skip the upfront `FetchToStore`; the materialized fallback lazy-fetches the source closure only when force, prune, or divergent refs require it ([#34](https://github.com/entireio/git-sync/pull/34))

### Fixed

- Sync into targets that share reachability with the source: tolerate pruned objects in the materialized walker, accept branch creates and `no-thin` targets in incremental relay, and surface diagnostic headers (`Cf-Ray`, `Server`, `X-Request-Id`) on opaque 5xx ([#33](https://github.com/entireio/git-sync/pull/33))

## [0.4.2] - 2026-04-30

First public release. `git-sync` mirrors refs from a source remote to a target
remote without a local checkout, streaming source packs directly into target
`receive-pack` whenever possible. The release covers the CLI, the library API,
and the protocol plumbing they share.

### Added

- `git-sync sync` — relay-based mirror that streams source `upload-pack`
  output into target `receive-pack` without materializing the object graph
  locally. Falls back to an in-memory `go-git` store, bounded by
  `--materialized-max-objects`, when relay is not eligible (force, prune,
  deletes, tag retargets) ([#1](https://github.com/entireio/git-sync/pull/1),
  [#2](https://github.com/entireio/git-sync/pull/2)).
- `git-sync replicate` and `git-sync plan --mode replicate` for
  source-authoritative, relay-only replication. Divergent branches and tags
  are retargeted against the source; `--prune` deletes orphan managed refs.
  Relay-only by design: no materialized fallback
  ([#4](https://github.com/entireio/git-sync/pull/4)).
- `git-sync plan` — preview the actions a `sync` or `replicate` would take,
  with structured JSON output suitable for automation.
- `git-sync bootstrap` — initial-seed path for empty targets, with adaptive
  batching, trunk-first planning to cut per-branch graph fetches, and
  resume-from-stale-temp-refs recovery
  ([#6](https://github.com/entireio/git-sync/pull/6)).
- `git-sync version` subcommand with build metadata
  ([#26](https://github.com/entireio/git-sync/pull/26)).
- Reusable Go library at `entire.io/entire/git-sync`. The stable surface
  (`Probe`, `Plan`, `Sync`, `Replicate`, typed results, auth and HTTP
  injection) lives at the module root; advanced controls (`Bootstrap`,
  `Fetch`, batching knobs, heap measurement) live in
  `entire.io/entire/git-sync/unstable`
  ([#3](https://github.com/entireio/git-sync/pull/3),
  [#17](https://github.com/entireio/git-sync/pull/17)).
- Git protocol v2 source-side support: `ls-refs`, `fetch` with v2
  acknowledgments and response-end handling, capability negotiation, and
  graceful fallback when the source does not advertise v2.
- Smart HTTP transport: pkt-line primitives, sideband demultiplexing, info/refs
  advertisement validation, smart endpoint path normalization, oversized
  packet rejection, empty pkt-line acceptance, and v2 fetch remote `ERR`
  packet handling.
- Optional info/refs redirect following on the source endpoint, exposed
  through the public `gitsync` API
  ([#9](https://github.com/entireio/git-sync/pull/9)).
- Git credential helper fallback and `--source-token` / `--target-token`
  flags for HTTPS auth.
- JSON output mode with a stable schema and camelCase keys across all
  commands ([#7](https://github.com/entireio/git-sync/pull/7)).
- Adaptive bootstrap batching: auto-subdivide on target body-size rejection,
  pre-check PACK header object count before pushing oversized batches, and
  shared `--max-pack-bytes` / `--target-max-pack-bytes` flags across `sync`,
  `replicate`, `plan`, and `bootstrap`.
- Sideband progress streamed to stderr when `-v` is set.
- Homebrew tap install via `brew tap entireio/tap && brew install --cask git-sync`
  ([#25](https://github.com/entireio/git-sync/pull/25)).
- GoReleaser-based release pipeline for cross-platform binaries
  ([#26](https://github.com/entireio/git-sync/pull/26)).
- Identical source and target endpoints are rejected before any network
  round-trips.
- Documentation set: `docs/usage.md`, `docs/architecture.md`,
  `docs/protocol.md`, `docs/testing.md`, plus README installation,
  quick-start, and FAQ ([#21](https://github.com/entireio/git-sync/pull/21),
  [#22](https://github.com/entireio/git-sync/pull/22),
  [#23](https://github.com/entireio/git-sync/pull/23)).
