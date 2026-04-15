# Embedding

`git-sync` can now be used as a library as well as a CLI.

For most embedders, there are two important rules:

- use `pkg/gitsync`
- avoid depending on `pkg/gitsync/unstable` unless you are acting like first-party tooling

## Stable vs Unstable

Use `pkg/gitsync` when you want a durable worker-facing API:

- `Probe`
- `Plan`
- `Sync`
- `Replicate`
- typed requests and results
- injected auth and HTTP client support

Use `pkg/gitsync/unstable` only when you need controls that are intentionally not yet stable:

- `Bootstrap`
- `Fetch`
- batching knobs
- heap measurement
- verbose execution controls
- other engine-adjacent tuning

The CLI and benchmark command use `pkg/gitsync/unstable` because they still need those controls. External workers should generally not.

## Worker Shape

A queue worker usually wants:

1. deserialize a job into source, target, scope, and policy
2. build a `gitsync.Client`
3. inject auth and an `http.Client`
4. call `Plan` or `Sync`
5. persist structured result data
6. decide success, retry, or escalation

Minimal example:

```go
package worker

import (
	"context"
	"net/http"

	"github.com/entirehq/git-sync/pkg/gitsync"
)

func runSync(ctx context.Context) error {
	client := gitsync.New(gitsync.Options{
		HTTPClient: &http.Client{},
		Auth: gitsync.StaticAuthProvider{
			Source: gitsync.EndpointAuth{Token: "source-token"},
			Target: gitsync.EndpointAuth{Token: "target-token"},
		},
	})

	result, err := client.Sync(ctx, gitsync.SyncRequest{
		Source: gitsync.Endpoint{URL: "https://github.example/source/repo.git"},
		Target: gitsync.Endpoint{URL: "https://git.example/target/repo.git"},
		Scope: gitsync.RefScope{
			Branches: []string{"main"},
		},
		Policy: gitsync.SyncPolicy{
			IncludeTags: true,
			Protocol:    gitsync.ProtocolAuto,
		},
	})
	if err != nil {
		return err
	}

	_ = result
	return nil
}
```

## Auth Injection

`pkg/gitsync` uses one auth ownership model:

- requests carry endpoint identity
- `AuthProvider` resolves source and target auth

That avoids baking CLI-style precedence rules into request types.

Good uses of `AuthProvider`:

- resolve OAuth tokens from your worker secret store
- attach different credentials for source and target
- centralize token refresh or lookup logic

The simplest option is `gitsync.StaticAuthProvider`, but a real worker will usually implement `AuthProvider` itself.

## HTTP Injection

Pass an `*http.Client` through `gitsync.Options` when you need:

- explicit timeouts
- custom TLS or proxy config
- OTEL or tracing round-trippers
- test transports
- custom connection pooling behavior

`git-sync` clones and wraps the provided client internally so it can still collect transfer stats without mutating the caller's client directly.

## Result Handling

The stable `SyncResult` is organized for worker consumption:

- `Refs`
  per-ref outcomes and reasons
- `Counts`
  aggregate applied/skipped/blocked/deleted totals
- `Execution`
  protocol, `operation_mode` (sync or replicate), relay summary,
  `transfer_mode` (the engine path that executed), and batch summary
- `Stats`
  transfer counters when requested
- `Measurement`
  only where exposed by the stable surface

That gives a worker enough structure to:

- persist job history
- emit metrics
- log ref-level outcomes
- make retry/escalation decisions

## Retry Guidance

Treat these differently:

- request construction and auth errors
  Usually configuration or secret-resolution issues. Retry only if your system expects credentials to become valid asynchronously.
- transport or remote errors returned from `Sync`
  Usually retryable depending on your queue policy and remote failure mode.
- successful `SyncResult` with blocked refs
  This is usually not a transport retry. It is a policy or repo-state outcome and should often be surfaced to operators.

For many workers, a useful pattern is:

- retry on returned `error`
- do not blindly retry on `Counts.Blocked > 0`
- log `Execution.OperationMode`, `Execution.TransferMode`, and
  `Execution.Reason` for operator visibility

## What Not To Depend On

If you want stability, do not build external worker logic around:

- batching thresholds
- max-pack controls
- materialized-object limits
- temp refs
- exact relay strategy names beyond coarse execution summary

Those are implementation details or advanced controls that currently belong in `pkg/gitsync/unstable`, not the stable embedding contract.
