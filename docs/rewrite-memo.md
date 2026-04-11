# git-sync Rewrite Memo

This memo captures the architectural conclusions behind the rewrite branch. It is meant to be read alongside [rewrite-issue-list.md](/Users/soph/Work/entire/devenv/git-sync/docs/rewrite-issue-list.md).

## Why Rewrite Instead Of Patch In Place

The core idea in `git-sync` is correct, but the current implementation has crossed the point where targeted fixes alone will keep the design clean.

The repo has three simultaneous realities:

- The product model is good.
- There are concrete correctness bugs and scaling issues.
- Too many concerns are collapsed into one implementation file and one package shape.

That combination makes a rewrite branch reasonable. The goal is not novelty. The goal is to:

1. Preserve the correct product decisions.
2. Remove accidental complexity.
3. Make edge-case correctness and performance behavior explicit.

## Current System Summary

The current code is a remote-to-remote Git mirroring CLI over smart HTTP.

Main execution modes:

- `bootstrap`
  - empty-target relay
- `sync`
  - normal planning and reconciliation
- incremental relay
  - narrow fast-path for safe updates
- materialized fallback
  - decode/repack path when relay is not safe
- batched bootstrap
  - large initial migration fallback

Most logic currently lives in:

- [cmd/git-sync/main.go](/Users/soph/Work/entire/devenv/git-sync/cmd/git-sync/main.go)
- [internal/syncer/syncer.go](/Users/soph/Work/entire/devenv/git-sync/internal/syncer/syncer.go)
- [internal/syncer/protocol_v2.go](/Users/soph/Work/entire/devenv/git-sync/internal/syncer/protocol_v2.go)

## What The Current Implementation Gets Right

### 1. Relay-first is the right core design

The most important idea in this repo is the relay model:

- fetch a pack from source
- avoid decoding and re-encoding when safe
- stream it into target

That is the right shape for large remote-to-remote mirroring.

### 2. Bootstrap as a separate strategy is correct

An empty-target initial sync is materially different from incremental reconciliation. Treating bootstrap as its own path is the right call.

### 3. Incremental relay should remain intentionally narrow

The code is right not to force every update into a relay path. Narrow strategy selection is preferable to fragile protocol cleverness.

### 4. Integration-heavy testing is the right style

This tool is protocol-heavy. Integration tests and `git-http-backend` style validation are more valuable here than a mostly unit-test-only suite.

### 5. Machine-readable output is worth preserving

The typed result model and JSON output are good product choices and should survive the rewrite.

## What Is Structurally Wrong In The Current Shape

### 1. Too many concerns live in one file

[internal/syncer/syncer.go](/Users/soph/Work/entire/devenv/git-sync/internal/syncer/syncer.go) currently mixes:

- planning
- strategy selection
- bootstrap batching
- push mechanics
- transport setup
- auth lookup
- token refresh
- stats
- measurement

That concentration is the main architectural problem.

### 2. Transport logic is split across two models

The code partially relies on `go-git` transport and partially bypasses it with custom smart-HTTP and protocol-v2 handling. That creates a dual-stack design:

- some transport concerns are implicit through `go-git`
- others are explicit in custom code

This is hard to reason about and hard to test consistently.

### 3. Capability handling is too scattered

Protocol feature checks should be centralized and typed. Right now they are spread across request builders and strategy paths.

### 4. Batch planning is too expensive and too implicit

Checkpoint planning currently leans on repeated fetch-and-discard probing. That makes the planner expensive, network-heavy, and difficult to reason about.

### 5. Validation is not front-loaded enough

Mappings and ref-policy decisions should fail before network activity. The current implementation allows some invalid configurations to survive into later execution phases.

## Rewrite Principles

### 1. Make strategy boundaries explicit

Each execution strategy should have:

- explicit inputs
- explicit capability requirements
- explicit output/result contract

### 2. Make validation a first-class phase

Before any network activity:

- normalize mappings
- reject invalid combinations
- reject duplicate targets
- construct the managed ref set

### 3. Make capability negotiation a first-class type

Instead of ad hoc checks across the codebase, capability negotiation should produce a typed description of what the server actually supports.

### 4. Prefer small packages over one “smart” package

The rewrite should reduce cross-cutting responsibility, not just move lines around.

### 5. Keep the product surface stable where possible

The rewrite should try to preserve:

- CLI shape
- JSON output contract where practical
- user-visible strategy semantics

## Proposed Package Model

### `internal/gitproto`

Responsibilities:

- pkt-line reading/writing
- smart HTTP request/response handling
- protocol v1 and v2 support
- capability negotiation
- ref advertisement
- fetch request building
- push request building

Why:
- The current code already wants this abstraction.
- The rewrite should own the HTTP protocol layer directly instead of mixing custom transport with `go-git` transport behavior.

### `internal/planner`

Responsibilities:

- mapping normalization
- mapping validation
- desired ref construction
- prune policy
- action planning
- checkpoint planning for batching

Why:
- This is the highest-leverage pure logic in the system.
- It should be testable without HTTP.

### `internal/auth`

Responsibilities:

- credential source ordering
- git credential helper integration
- Entire token lookup
- refresh behavior
- token store backends

Why:
- Auth currently adds a lot of noise to orchestration.
- It should be isolated and explicit.

### `internal/strategy/bootstrap`

Responsibilities:

- one-shot bootstrap relay
- batched bootstrap
- checkpoint execution
- temp ref cutover

Why:
- Bootstrap is already a separate product concept and deserves separate execution code.

### `internal/strategy/incremental`

Responsibilities:

- incremental relay eligibility
- relay fetch/push execution

Why:
- This path has distinct capability and safety rules.

### `internal/strategy/materialized`

Responsibilities:

- local object materialization
- object closure
- encode/repack push

Why:
- This path is meaningfully different from relay paths and should be isolated from them.

### `internal/syncer`

Responsibilities:

- top-level orchestration
- session assembly
- choosing a strategy
- collecting results

Why:
- The orchestrator should become smaller, not smarter.

## Recommended Interfaces

These do not need to be exact, but the rewrite should aim for this level of separation.

### Source-side interfaces

- `RefLister`
- `PackFetcher`
- `CommitGraphFetcher`

### Target-side interfaces

- `RefAdvertiser`
- `PackPusher`
- `CommandPusher`

### Planning interfaces

- `MappingValidator`
- `Planner`
- `CheckpointPlanner`

The point is not abstraction for its own sake. The point is to make:

- planning testable without HTTP
- protocol testable without planning
- strategy testable without CLI parsing

## go-git In The Rewrite

Recommended position:

- own the smart HTTP protocol layer directly
- keep `go-git` packfile/object support only where it still buys value

Reasoning:

- The current code already bypasses `go-git` transport in important places.
- Owning the HTTP/protocol layer removes the dual-stack problem.
- Keeping packfile/object utilities can still be pragmatic for the materialized fallback path.

## Logging

Replace ad hoc `progressf`-style stderr logging with structured logging.

Desired properties:

- log levels
- structured fields
- easy correlation of branch/batch/protocol information
- consistent formatting across text and JSON-oriented automation use

`slog` is the obvious standard-library fit.

## Performance Position

The rewrite should not try to make every path maximally optimal on day one. It should make performance costs visible and bounded.

Priority performance goals:

- avoid repeated fetch-and-discard pack probing
- reduce unbounded buffering
- make non-relay cost explicit
- keep relay path cheap

It is acceptable for the materialized fallback path to remain expensive if the rewrite makes those limits explicit and predictable.

## Testing Position

The rewrite should preserve the current integration-heavy style and add stronger unit coverage around pure logic.

Desired balance:

- unit tests for planning, validation, checkpointing, and protocol parsing
- integration tests for end-to-end smart HTTP behavior
- `git-http-backend` coverage for realistic wire compatibility
- race tests
- benchmarks for the expensive paths

## Migration Strategy

This rewrite branch should not start by porting every function. It should start by rebuilding the design boundaries.

Recommended order:

1. Define package boundaries and interfaces.
2. Rebuild validation and planning first.
3. Rebuild protocol handling and capability negotiation.
4. Rebuild bootstrap relay.
5. Rebuild incremental relay.
6. Rebuild materialized fallback.
7. Rebuild auth handling.
8. Port and expand the tests.
9. Compare rewrite behavior against the current branch.

## Comparison Criteria Against Current Branch

The rewrite branch should be judged on:

- correctness on the current integration scenarios
- better validation failure behavior
- clearer package boundaries
- lower accidental complexity
- clearer capability handling
- better behavior under race testing
- more explicit performance tradeoffs

The rewrite does not need to preserve every internal mechanism. It needs to preserve the right product behavior and improve the maintainability and safety profile.

## Bottom Line

This repo does not need a different product idea. It needs a cleaner implementation shape.

The current system discovered the right execution model:

- relay when safe
- bootstrap separately
- fall back when necessary

The rewrite branch should preserve that core and rebuild the internals so the next round of work is adding capability, not fighting structural debt.

