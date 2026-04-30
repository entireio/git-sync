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

## Benchmarking

`gitsync-bench` runs repeatable empty-target benchmarks against a source repository. It creates a fresh bare target for each run and reports wall-clock time plus internal `syncer` measurement data.

Build it with:

```bash
go build -o /tmp/gitsync-bench ./cmd/gitsync-bench
```

Example against a local mirror:

```bash
/tmp/gitsync-bench \
  --scenario bootstrap \
  --source-url /tmp/gitsync-bench/kubernetes.git \
  --repeat 3 \
  --target-max-pack-bytes 104857600 \
  --stats \
  --json
```

The JSON report includes per-run results, aggregate min/avg/max timings, batch counts for batched runs, heap peaks, and relay modes seen across successful runs. If `--source-url` is a filesystem path, the tool converts it to `file://...` automatically.

Use `--keep-targets` to retain generated bare targets under `--work-dir` for inspection. For large real-repo runs, prefer a local mirror instead of benchmarking directly against a hosted remote.

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
