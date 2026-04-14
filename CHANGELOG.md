# Changelog

Entries are grouped per release. Unreleased changes sit at the top.

## Unreleased

### Added

- `git-sync replicate` subcommand and `git-sync plan --mode replicate` for
  source-authoritative, relay-only replication. Divergent branches and tags
  are retargeted against the source; `--prune` deletes orphan managed refs.
  Relay-only by design: no materialized fallback. Replicate works against
  targets that advertise `no-thin` (including any server built on go-git's
  receive-pack, e.g. `entire-server`) because the relayed pack is always
  self-contained — our upload-pack client never requests the `thin-pack`
  capability from the source.
- `gitsync.Client.Replicate` on the stable embedding surface.
- `gitsync.OperationMode`, `gitsync.ModeSync`, `gitsync.ModeReplicate`, and
  `SyncPolicy.Mode` for selecting the mode from library callers.
- `--max-pack-bytes` and `--target-max-pack-bytes` flags on `sync`,
  `replicate`, and `plan`. Previously only `bootstrap` exposed them, but
  replicate's bootstrap-fallback path internally honors both — without
  these flags, users couldn't split a huge initial replicate push into
  tractable receive-pack POSTs for size-limited targets. The unstable
  library `buildSyncConfig` now forwards `MaxPackBytes` and
  `TargetMaxPackBytes` from `AdvancedOptions` to `syncer.Config`.

### Changed (stable API, breaking)

- The stable `SyncResult.Execution` summary has a new field layout:
  - Added `execution.operation_mode` (`"sync"` or `"replicate"`) to describe
    the high-level product mode the caller requested.
  - Renamed `execution.mode` to `execution.transfer_mode` to describe the
    low-level engine path that actually executed (`incremental-relay`,
    `materialized`, `bootstrap-relay`, `replicate`, etc.). The previous
    `mode` key is no longer emitted.

  External embedders that parse `execution.mode` must switch to
  `execution.transfer_mode`. There is no compatibility alias; the field was
  renamed in place.

- `syncer.Result` (the CLI-level JSON shape) gains a top-level
  `operation_mode` field alongside the existing relay fields. Callers
  consuming the flat CLI JSON can use it to distinguish sync vs replicate
  results.

### Fixed

- Work around a go-git v6 upload-pack bug where the server emits two
  consecutive `NAK` pktlines in stateless-RPC mode when the client sends
  haves that are not reachable from any want. The second NAK would
  otherwise be misread by the sideband demuxer as a frame with channel
  byte `'N'` ("unknown channel NAK"). `internal/gitproto.fetchPackV1` and
  `fetchToStoreV1` now drain trailing NAKs before handing off to the
  demuxer. See the corresponding go-git fix
  (`plumbing/transport: don't emit a second NAK after encoding empty ACKs`).
