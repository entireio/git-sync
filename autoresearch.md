# Autoresearch: syncer benchmark execution overhead

## Objective
Optimize the rewrite branch's end-to-end sync execution overhead as measured by the in-process `internal/syncer` benchmarks. The target workload is the trio of execution-path benchmarks that exercise bootstrap relay, incremental relay, and materialized fallback. We care about lowering aggregate benchmark time without breaking behavior or widening the product surface.

## Metrics
- **Primary**: `total_ms` (ms, lower is better) — median of repeated benchmark runs, summing the three `internal/syncer` benchmark `ns/op` values and converting to milliseconds
- **Secondary**: `bootstrap_ms`, `incremental_ms`, `materialized_ms`, `bootstrap_alloc_b`, `incremental_alloc_b`, `materialized_alloc_b`, `go_test_elapsed_s` — to detect path-specific regressions and allocation tradeoffs

## How to Run
`./autoresearch.sh` — outputs structured `METRIC name=value` lines.

## Files in Scope
- `internal/syncer/` — top-level orchestration on the hot benchmarked path
- `internal/strategy/bootstrap/` — bootstrap relay path used by benchmark coverage
- `internal/strategy/incremental/` — incremental relay path used by benchmark coverage
- `internal/strategy/materialized/` — fallback path if shared orchestration changes affect it
- `internal/gitproto/` — fetch/push helpers and pack wrappers used by the strategies
- `internal/planner/` — planning helpers if they materially affect sync benchmarks
- `internal/convert/` — conversion helpers on the execution path
- `cmd/git-sync-bench/` — instrumentation only if needed for diagnosis

## Off Limits
- Public CLI behavior and JSON output contracts unless strictly performance-neutral
- New third-party dependencies
- Unrelated rewrite cleanup not justified by benchmark wins
- Default branch comparison work; this session targets the rewrite branch only

## Constraints
- Keep user-visible behavior intact
- Prefer simpler code when benchmark results are equal
- Run `go test ./...` manually before keeping non-trivial changes; the autoresearch checks hook appears unreliable in this repo and was timing out despite fast local completion
- Benchmark noise is expected; confirm marginal wins before keeping them

## What's Been Tried
- Two initial baseline attempts produced valid benchmark metrics but `autoresearch.checks.sh` timed out under `run_experiment` even though `go test ./...` completed quickly when run directly. Treat the hook as unreliable here and run tests manually before keeping meaningful code changes.
- Initial code reading suggests avoidable map/slice conversion churn on the relay paths (`planner.DesiredSubset` -> `convert.DesiredRefs` -> `gitproto.ToPushCommands`) may be one of the few low-risk hot-path opportunities worth testing first.
- The benchmarked execution paths are already fairly small, so broad architectural rewrites are less likely to pay off than removing repeated tiny allocations or duplicated work on the strategy path.
