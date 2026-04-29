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

Batch-planning sensitivity coverage:

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

- `mise run test` — default suite
- `mise run test:ci` — default suite with race detection
- `mise run test:git-http-backend` — `git-http-backend` end-to-end
- `mise run test:linux-smoke` — live Linux bootstrap smoke
- `mise run test:linux-smoke:batched` — live Linux batched bootstrap smoke

`test:linux-smoke` and `test:linux-smoke:batched` use a disposable local bare Git target served through `git-http-backend`. They do not require any external service beyond the public source remote.

## TLS Overrides

For local or self-signed targets:

- `--source-insecure-skip-tls-verify`
- `--target-insecure-skip-tls-verify`
- `GITSYNC_SOURCE_INSECURE_SKIP_TLS_VERIFY=true`
- `GITSYNC_TARGET_INSECURE_SKIP_TLS_VERIFY=true`
