# Protocol

This document is a code-grounded walkthrough of the Git wire protocol pieces that `git-sync` implements directly: smart HTTP transport, pkt-line framing, capability negotiation, sideband multiplexing, and the relay path that streams a source pack into target `receive-pack` without local materialization.

It is aimed at contributors and embedders who want to understand *why* the implementation looks the way it does. For higher-level operational behavior, see [architecture.md](architecture.md).

## Scope

Covered:

- Smart HTTP transport (`info/refs` discovery + RPC POST endpoints)
- Pkt-line framing
- Sideband-64k multiplexing
- Capability negotiation, both v1 (advertised on the first ref line) and v2 (`ls-refs` / `fetch` capability advertisement)
- Source fetch and target push request/response shape
- The relay path: how a fetched source pack is forwarded into target `receive-pack` without decoding object graphs locally
- Auth, TLS, redirect handling

Out of scope (with pointers):

- The pack format itself (object types, deltas, index format) — see [Git's pack-format docs](https://git-scm.com/docs/pack-format)
- Dumb HTTP — `git-sync` does not support it
- SSH transport — `git-sync` is HTTPS-only
- Bundle URI, partial clones, and other newer extensions

## Smart HTTP Overview

`git-sync` talks to two RPC services per remote:

| Service          | Discovery endpoint                            | RPC endpoint        | Direction             |
|------------------|-----------------------------------------------|---------------------|-----------------------|
| `git-upload-pack` | `GET /info/refs?service=git-upload-pack`     | `POST /git-upload-pack` | source: list refs, fetch objects |
| `git-receive-pack` | `GET /info/refs?service=git-receive-pack` | `POST /git-receive-pack` | target: list refs, push objects  |

The `/info/refs` GET serves two purposes: it advertises the available refs (in v1) or the v2 capability set, and it acts as the negotiation handshake — the response tells the client which protocol version the server speaks and which capabilities are available. The subsequent POST to the RPC endpoint carries the request body in `application/x-<service>-request` format and receives `application/x-<service>-result` back.

`git-sync` is smart-HTTP-only. Dumb HTTP is not supported because relay-friendly negotiation depends on capability discovery and `have`/`want` exchange that dumb HTTP does not provide.

The transport implementation lives in `internal/gitproto/smarthttp.go`. The two main entry points are `RequestInfoRefs` (the GET) and `PostRPC` / `PostRPCStream` / `PostRPCStreamBody` (the POST, with buffered or streaming response bodies).

## Pkt-Line Framing

Every smart HTTP request and response body is a sequence of pkt-line packets. Each packet begins with a 4-byte ASCII hex length prefix, followed by the payload.

Three special length values do not carry a payload:

| Marker | Hex     | Constant                 | Meaning           |
|--------|---------|--------------------------|-------------------|
| flush  | `0000`  | `PacketFlush`            | end of section / end of stream |
| delim  | `0001`  | `PacketDelim`            | section delimiter (v2) |
| end    | `0002`  | `PacketResponseEnd`      | end of v2 response |

Anything else is parsed as `0xXXXX` and indicates a packet of `n - 4` payload bytes.

Implementation: `internal/gitproto/pktline.go`. `PacketReader` reuses a fixed 4-byte header buffer and a growable payload buffer per call to `ReadPacket()` to keep allocations down on long pkt-line streams. `EncodeCommand` builds v2 command request bodies as `command=<name>` + capability args + optional delim + command args + flush. `FormatPktLine` is a small helper for building one-off payload packets.

### Sideband-64k

When the server advertises `side-band-64k`, the response payload is multiplexed across three logical channels. The first byte of each pkt-line payload is the channel selector:

| Channel | Meaning                  |
|---------|--------------------------|
| `0x01`  | pack data                |
| `0x02`  | progress text (stderr)   |
| `0x03`  | fatal error              |

`git-sync` always negotiates `side-band-64k` when available. `PreferredSideband` in `capability.go` enforces preferring `side-band-64k` over the older `side-band` capability. The demux happens in the fetch path; see "Source Fetch" below for how the pack stream is unwrapped.

## Capability Negotiation

### Source side (upload-pack)

For v1, capabilities are advertised at the end of the first ref line in the `info/refs` response, separated from the ref by a NUL byte. `git-sync` parses these via `go-git`'s `packp.AdvRefs`.

For v2, capabilities are advertised as their own pkt-line section after a `version 2\n` payload. `DecodeV2Capabilities` in `internal/gitproto/capability.go` parses this into `V2Capabilities{Caps: map[string]string}`. Each capability is either a bare name or `name=value`.

The capabilities `git-sync` cares about on the source side:

- `agent` — informational; `git-sync` echoes its own agent string back
- `side-band-64k` — multiplexed responses with progress
- `multi_ack_detailed`, `no-done` — fetch negotiation efficiency
- `ofs-delta` — pack encoding capability
- `fetch` (v2) — advertised features include `shallow`, `filter`, `wait-for-done`. `FetchSupports` parses the space-separated feature list.
- `ls-refs` (v2) — required for v2 ref discovery

### Target side (receive-pack)

`TargetFeaturesFromAdvRefs` in `internal/gitproto/target_features.go` summarizes the receive-pack advertisement into a typed `TargetFeatures` struct:

```go
type TargetFeatures struct {
    Known        bool
    DeleteRefs   bool
    NoThin       bool
    OFSDelta     bool
    ReportStatus bool
    Sideband     bool
    Sideband64k  bool
}
```

`git-sync` never requests `thin-pack` on the source side, so the source pack is always self-contained. That makes the relayed pack safe to push to a target regardless of its `no-thin` advertisement. The relay paths still differ on what they do when the target advertises `no-thin`: see "Why `no-thin` matters" under Target Push.

`DeleteRefs` gates `--prune` and explicit deletes. `ReportStatus` is required to learn which ref updates the target accepted. `OFSDelta` affects pack encoding compatibility. `Sideband` / `Sideband64k` allow the target to multiplex its own response.

## Protocol v1 vs v2

Protocol v2 was introduced to make ref discovery cheaper (you can ask for a ref prefix instead of receiving the full advertisement) and to make the RPC body more structured.

| Aspect                | v1                                          | v2                                              |
|-----------------------|---------------------------------------------|-------------------------------------------------|
| Discovery handshake   | full ref advertisement on `info/refs`       | capability advertisement only                    |
| Ref listing           | implicit in discovery                       | explicit `command=ls-refs` RPC                   |
| Fetch                 | `want`/`have`/`done` lines on `upload-pack` | `command=fetch` RPC with capability args         |
| Filtering (`tree:0`)  | not standardized                            | supported via `fetch` capability `filter` feature |
| Push                  | command list + pack on `receive-pack`       | (not yet standardized; not used)                 |

`git-sync` uses v2 on the source side when supported, and v1 on the target side always. The reasons:

- **Source-side v2** enables features `git-sync` actually uses: `ls-refs` for cheaper ref listing, and `fetch` with `filter=tree:0` for the commit-graph-only fetches that batched bootstrap planning relies on.
- **Target-side push** stays on v1 because v2 receive-pack is not yet adopted broadly enough to rely on, and because `git-sync` already needs explicit command construction and streaming control on the push path.

The CLI flag is `--protocol auto|v1|v2`. `auto` (the default) tries source-side v2 first via the `Git-Protocol: version=2` header, and falls back to v1 if the server doesn't acknowledge v2 in its response. `v2` requires the source to negotiate v2 and fails otherwise. `v1` skips the v2 attempt.

The negotiation is done in `internal/gitproto/refs.go` via `ListSourceRefs(ctx, conn, protocolMode, refPrefixes)`, which returns a `RefService` that records the negotiated protocol and either the v1 advertisement or the v2 capabilities. Subsequent fetches go through that `RefService`.

## HEAD Symref Discovery

When the source advertises HEAD as a symbolic ref, `git-sync` records what branch it points to. This is used by batched bootstrap planning as a *trunk hint*: the trunk branch is planned first so its commit graph becomes a stop-set for subsequent branches' first-parent walks, dramatically cutting per-branch graph fetches on multi-branch repos.

The hint is exposed as `RefService.HeadTarget plumbing.ReferenceName`. Empty value means "detached HEAD or no symref advertised", in which case the caller falls back to the original branch order.

### v1

In v1, HEAD's symref target is announced as a `symref=` capability on the first ref line:

```
<hash> HEAD\0... symref=HEAD:refs/heads/main side-band-64k ofs-delta ...
```

`headTargetFromAdv` walks the parsed `capability.SymRef` values and returns the right-hand side of `HEAD:<branch>`.

### v2

In v2, the symref-target attribute is delivered as part of the `ls-refs` response. To get it, `git-sync` always requests HEAD and the `symrefs` argument:

```
command=ls-refs
agent=git-sync/...
0001
peel
symrefs
ref-prefix HEAD
ref-prefix refs/heads/
ref-prefix refs/tags/
0000
```

The `peel` and `symrefs` arguments are ls-refs request features that ask the server to include peeled object IDs (for tags) and symref-target attributes. `ref-prefix HEAD` is unconditionally included so HEAD shows up in the response even when the caller only asked for `refs/heads/` or `refs/tags/`.

The server's HEAD line then looks like:

```
<hash> HEAD symref-target:refs/heads/main
```

`decodeV2LSRefs` parses the line, extracts the `symref-target:` attribute, and returns it as `headTarget` alongside the ref slice. HEAD itself is filtered out of the returned refs because it is a symbolic ref, not a real one — matching v1 behavior where symrefs are filtered out by the downstream `RefHashMap`.

### Consumers

The trunk hint is read from `RefService.HeadTarget` and passed into `bootstrap.Params.SourceHeadTarget` (the wiring lives in `internal/syncer/syncer.go`). `orderTrunkFirst` (in `internal/strategy/bootstrap/bootstrap.go`) reorders the desired-ref list so the trunk is planned first. If the trunk is not in the desired set (filtered out by `--branch` or `--map`), the original order is preserved and the trunk-first optimization is skipped.

## Source Fetch

### v1

Request body format (one pkt-line per element):

```
want <hash> <capabilities-on-first-line>\n
want <hash>\n
...
0000
have <hash>\n
...
done\n
```

Capabilities are appended to the first `want` line, separated from the hash by a space. `git-sync` requests `agent=...`, the preferred sideband (`side-band-64k` when supported, otherwise `side-band`), `ofs-delta` when supported, and `include-tag` when the request asks for tags. It adds `no-progress` unless `Verbose` is set on the `RefService`. `git-sync` deliberately does *not* request `thin-pack` — see "Why no-thin matters" under Target Push.

Response: a pkt-line stream that begins with NAK / ACK negotiation packets and transitions into pack data on sideband channel `0x01`. Channel `0x02` carries progress text; channel `0x03` is fatal.

### v2

Request body is built by `EncodeCommand("fetch", capabilityArgs, commandArgs)`:

```
command=fetch\n
agent=git-sync/...\n
0001
want <hash>\n
have <hash>\n
done\n
0000
```

The capability section is separated from the command-arg section by a delim packet (`0001`), and the whole body is terminated by a flush packet (`0000`). Filter features like `filter tree:0` go in the command-arg section.

Response: pkt-line sections delimited by `0001`. The interesting section is `packfile`, where each subsequent pkt-line carries one byte of channel selector + payload (same sideband-64k encoding as v1).

### Demux

`internal/gitproto/fetch.go` is responsible for unwrapping the response into a raw pack stream. It:

1. Reads pkt-lines from the buffered `PacketReader`.
2. Skips ACK/NAK negotiation packets (v1) or section delimiters until reaching `packfile` (v2).
3. For each subsequent payload pkt-line, dispatches the first byte to the right channel: `0x01` → forward to the pack consumer, `0x02` → progress (stderr if `Verbose`, otherwise discard), `0x03` → fail.
4. Stops on the next flush packet.

Once the stream switches to pack mode, subsequent reads bypass pkt-line framing where possible. The reader exposes `BufReader()` so the caller can hand the underlying buffered reader directly to a pack-consumer that reads raw bytes.

## Target Push

`receive-pack` POST body is, in order:

1. **Command list**, one pkt-line per ref update:
   ```
   <old-hash> <new-hash> <refname>\0<capabilities>\n
   ```
   Capabilities are appended to the first command after a NUL byte. `git-sync` requests `report-status` and the preferred sideband (`side-band-64k` when available, otherwise `side-band`); `delete-refs` is added when the command list contains a delete. `ofs-delta` is consulted from the target advertisement to choose pack-encoding behavior in `useRefDeltas`, but is not itself requested as a wire capability.
2. **Flush packet** (`0000`) terminating the command section.
3. **Raw pack bytes**. The pack itself is NOT pkt-line framed — the receive-pack server reads the pack directly off the request body after the flush.

### Why `no-thin` matters

A "thin pack" is one that contains delta objects whose base is not in the pack itself. The receiver is expected to resolve those bases against existing target objects. Thin packs are smaller on the wire but require the receiver to reach into the existing object database during indexing. When a `receive-pack` server advertises `no-thin`, it is signalling that it does not support thin packs — the client must send a self-contained pack.

`git-sync` always sends self-contained packs because it never requests the `thin-pack` capability on `upload-pack` (see the comment in `internal/gitproto/fetch.go`). That means the relayed pack is safe for both kinds of target. The relay paths still make different choices given the target's advertisement:

- **Replicate** explicitly tolerates `no-thin` and proceeds with relay. The comment in `internal/planner/relay.go` documents the reasoning: source never sends thin-pack, so the relayed pack is self-contained and safe.
- **Incremental relay** inside `sync` is conservative: it skips `no-thin` targets and falls back to the materialized path. This is a safety-margin choice; it could in principle be relaxed using the same argument as replicate.
- **Bootstrap** does not consult `no-thin` — empty-target relay is allowed regardless.

### report-status

After the pack is uploaded, the server responds with a `report-status` (or `report-status-v2`) section listing per-ref outcomes:

```
unpack ok\n
ok refs/heads/main\n
ng refs/heads/release pre-receive-hook-rejected\n
```

`git-sync` parses this in `internal/gitproto/push.go` and surfaces per-ref outcomes through the higher-level result types.

The push implementation lives in `internal/gitproto/push.go`.

## Relay Framing

The conceptual move that distinguishes `git-sync` from a `clone --mirror` + `push --mirror` workflow is **relay**: the source's pack bytes are forwarded into the target's `receive-pack` request body without going through a local object decode + repack cycle.

In framing terms:

```
source response stream                   target push request body
─────────────────────────────              ──────────────────────────
pkt-line(NAK/ACK or v2 sections)           pkt-line(command list)
pkt-line(0x02 progress) ─── stderr         pkt-line(flush)
pkt-line(0x01 [pack chunk]) ───────┐       raw pack bytes ◄───────────┐
pkt-line(0x01 [pack chunk]) ───────┼─►     raw pack bytes ◄───────────┤
pkt-line(0x01 [pack chunk]) ───────┘       raw pack bytes ◄───────────┘
pkt-line(flush)
```

The source's pack chunks come out one-per-pkt-line on sideband channel `0x01`. `git-sync` strips the pkt-line + sideband envelope and concatenates the raw pack bytes into the target push body, after the command list and flush. The pack itself is not re-encoded: byte for byte, the source-produced pack shows up on the target's wire.

This is why the relay paths (bootstrap, replicate, and the incremental relay path inside `sync`) avoid the in-memory cost that the materialized fallback pays. The local process never holds the object graph; it holds at most one pack chunk at a time.

### PACK header pre-check

The batched bootstrap path needs to make a sizing decision before committing to a full transfer. Every pack starts with a 12-byte header:

```
"PACK"  uint32 version  uint32 object_count
```

`checkPackSizeAndSubdivide` in `internal/strategy/bootstrap/bootstrap.go` peeks at the first 12 bytes of the unwrapped pack stream, multiplies the object count by a per-object byte estimate, and if the projected pack size exceeds `--target-max-pack-bytes`, aborts the fetch and inserts an additional checkpoint between the current `have` and the target tip — wasting 12 bytes of read instead of gigabytes of transfer.

The same code path also detects target-side body-size rejections after the fact: if the target rejects the push because the pack is too large, the planner inserts a midpoint checkpoint and retries. Both safeguards converge in O(log n) splits.

## Auth, TLS, Credential Helper

### Auth methods

`ApplyAuth` in `internal/gitproto/smarthttp.go` recognizes two auth method types from `go-git`:

- `*transporthttp.BasicAuth` — sets `Authorization: Basic <base64(user:pass)>`. For GitHub-style providers, the password slot carries the token (`username=git`, `password=$GITHUB_TOKEN`).
- `*transporthttp.TokenAuth` — sets `Authorization: Bearer <token>`. Used for providers that expect bearer tokens directly.

Resolution order (CLI side):

1. Explicit `--source-token`, `--source-bearer-token`, `--source-username` flags (and target equivalents)
2. `GITSYNC_*` environment variables (`GITSYNC_SOURCE_TOKEN`, `GITSYNC_TARGET_BEARER_TOKEN`, etc.)
3. `git credential fill` helper lookup, for `http://` and `https://` URLs
4. Anonymous (no `Authorization` header)

Library callers inject auth via the `AuthProvider` interface in `entire.io/entire/git-sync` instead.

### TLS

`NewHTTPTransport(skipTLS bool)` returns a clone of `http.DefaultTransport` with `InsecureSkipVerify` flipped on when requested. The CLI flags are `--source-insecure-skip-tls-verify` and `--target-insecure-skip-tls-verify`; environment overrides are `GITSYNC_*_INSECURE_SKIP_TLS_VERIFY=true|1|yes|on`.

This is intended for local self-signed targets. Do not use it against the public internet.

## /info/refs Redirect Behavior

Some hosting providers respond to `/info/refs` with a 307 redirect to a different host (typically a regional replica or a canonicalized URL). Vanilla `git` follows the redirect for the discovery GET but also retargets the subsequent RPC POSTs at the redirect target.

`git-sync` exposes this as the `FollowInfoRefsRedirect` field on `gitproto.Conn`, and as the CLI flags `--source-follow-info-refs-redirect` and `--target-follow-info-refs-redirect`:

- **Off (default)**: redirects are followed for the GET, but the subsequent RPC POSTs go to the original `Endpoint.Host`. This preserves stable behavior for callers that build URLs ahead of time.
- **On**: after `RequestInfoRefs` follows redirects, `Endpoint.Scheme` and `Endpoint.Host` are rewritten to the final URL's scheme and host. `Endpoint.Path` is never modified — it still contains the repo path.

Library callers can set this via `gitsync.Endpoint{FollowInfoRefsRedirect: true}`.

## Stats Counters and Common Failures

When `--stats` is set, every HTTP round-trip is annotated with the `X-Git-Sync-Stats-Phase` header (`StatsPhaseHeader` in smarthttp.go), e.g. `git-upload-pack info-refs`, `git-receive-pack push`, etc. The instrumented round-tripper accumulates per-phase counters:

- request count
- bytes sent / received
- `want` count, `have` count
- target push command count

These show up as the `stats` block in `--json` output and as a summary table in text output.

Bounded reads protect the local process:

- `info/refs` responses are capped at 64 MiB
- Buffered RPC responses (`PostRPC`) are capped at 128 MiB

Streaming RPC bodies (`PostRPCStreamBody`) bypass these caps because the consumer is the relay path, which reads pack chunks incrementally and forwards them on.

### Common failure surfaces

- **v2 negotiation failed**: source returned a v1 advertisement when `--protocol v2` was forced. `auto` falls back; `v2` errors out. Causes: older server, intermediate proxy stripping the `Git-Protocol` header.
- **Target rejected push (body too large)**: relay produced a pack larger than the target's request body limit. Solutions: lower `--target-max-pack-bytes` for batched bootstrap, or rerun on a target with a higher limit. Detected by `isTargetBodyLimitError` in `internal/strategy/bootstrap/bootstrap.go`.
- **Target advertises `no-thin`**: the incremental relay path inside `sync` skips it and falls back to the materialized path. `replicate` proceeds anyway.
- **Redirect chain mismatch**: the GET redirects to host X, but a subsequent RPC POST gets a different redirect or a 404 because the server expected sticky sessions. Resolution: set `FollowInfoRefsRedirect=true` so RPC POSTs go directly to the post-redirect host.
- **Auth 401 / 403**: 401 generally indicates missing or wrong credentials (will retry with credential helper if configured); 403 indicates the credentials were accepted but lack permission for the requested action.

## Related

- [architecture.md](architecture.md) — package layout and where each protocol piece lives
- [testing.md](testing.md) — test suites and integration coverage
