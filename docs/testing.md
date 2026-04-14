# Testing

## Default Suite

The default test suite uses in-process smart HTTP servers and does not require a local listener:

```bash
env GOCACHE=/tmp/go-build go test ./...
```

## `git-http-backend` End-To-End Tests

Optional end-to-end write test against the system `git-http-backend`:

```bash
env GOCACHE=/tmp/go-build GITSYNC_E2E_GIT_HTTP_BACKEND=1 go test ./internal/syncer -run TestRun_GitHTTPBackendSync -v
```

That path exercises real smart HTTP fetch and push with a local bare source repo and a local bare target repo.

Dedicated batched bootstrap coverage:

```bash
env GOCACHE=/tmp/go-build GITSYNC_E2E_GIT_HTTP_BACKEND=1 go test ./internal/syncer -run TestBootstrap_GitHTTPBackendBatchedBranch -v
```

Batch-planning sensitivity experiment for `#14`:

```bash
env GOCACHE=/tmp/go-build GITSYNC_E2E_GIT_HTTP_BACKEND=1 go test ./internal/syncer -run TestBootstrap_GitHTTPBackendBatchedPlanningTracksBatchLimit -v
```

That test uses a real `git-http-backend` source/target pair and checks that a smaller `--target-max-pack-bytes` planning limit produces at least as many planned checkpoints as a larger one, while still planning to the branch tip.

## Live Linux Smokes

Optional live Linux bootstrap smoke:

```bash
env GOCACHE=/tmp/go-build GITSYNC_E2E_LIVE_LINUX=1 go test ./internal/syncer -run TestBootstrap_LiveLinuxSource -v
```

Batched variant:

```bash
env GOCACHE=/tmp/go-build GITSYNC_E2E_LIVE_LINUX=1 go test ./internal/syncer -run TestBootstrap_LiveLinuxSourceBatched -v
```

These are useful for large-source relay and memory checks while keeping the target local and disposable.

## `mise` Tasks

- `mise run test:linux-smoke`
- `mise run test:linux-smoke:batched`
- `mise run test:entire-local-smoke`
- `mise run test:entire-local-smoke:linux`
- `mise run test:entire-local-smoke:linux:single`

`test:linux-smoke` and `test:linux-smoke:batched` use a disposable local bare Git target served through `git-http-backend`. They do not talk to a running Entire instance.

## Entire Local Smoke

The Entire local smoke expects:

- a running Entire local instance reachable at `GITSYNC_E2E_ENTIRE_BASE_URL` or `ENTIRE_BASE_URL`
- an authenticated local Entire CLI session for that host, so the smoke can discover the active user and OAuth token from `hosts.json` and the keyring
- `entiredb` on `PATH`, or `GITSYNC_E2E_ENTIREDB_BIN` pointing to it

Useful overrides:

- `GITSYNC_E2E_ENTIRE_SOURCE_URL=https://github.com/entireio/cli.git`
- `GITSYNC_E2E_ENTIRE_BRANCH=main`
- `GITSYNC_E2E_ENTIRE_REPO=git-sync-smoke`
- `GITSYNC_E2E_ENTIRE_USERNAME=...`
- `GITSYNC_E2E_ENTIRE_TOKEN=...`
- `GITSYNC_E2E_ENTIRE_SKIP_TLS_VERIFY=true`

To sync the public Linux repo into a local Entire instance:

```bash
mise run test:entire-local-smoke:linux
```

That task sets:

- `GITSYNC_E2E_ENTIRE_SOURCE_URL=https://github.com/torvalds/linux.git`
- `GITSYNC_E2E_ENTIRE_BRANCH=master`
- `GITSYNC_E2E_ENTIRE_PROTOCOL=v2`
- `GITSYNC_E2E_ENTIRE_BATCH_MAX_PACK_BYTES=536870912`

The batched default matters for Entire targets with request body limits around `2 GiB`; a single Linux bootstrap push can exceed that. If you explicitly want the old single-pack behavior:

```bash
mise run test:entire-local-smoke:linux:single
```

If you use `test:entire-local-smoke` directly, the test falls back to `https://github.com/entireio/cli.git` on `main` unless you override both the source URL and branch yourself.

The Entire-local smoke also accepts:

- `GITSYNC_E2E_ENTIRE_MAX_PACK_BYTES`
- `GITSYNC_E2E_ENTIRE_BATCH_MAX_PACK_BYTES`
- `GITSYNC_E2E_ENTIRE_PROTOCOL`

## TLS Overrides

For local or self-signed targets:

- `--source-insecure-skip-tls-verify`
- `--target-insecure-skip-tls-verify`
- `GITSYNC_SOURCE_INSECURE_SKIP_TLS_VERIFY=true`
- `GITSYNC_TARGET_INSECURE_SKIP_TLS_VERIFY=true`
