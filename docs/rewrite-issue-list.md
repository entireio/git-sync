# git-sync Rewrite Issue List

This document converts the review memo into a concrete issue list for a from-scratch rewrite branch.

The intent is not to patch the current architecture incrementally first. The intent is to:

1. Preserve the parts of the current implementation that are directionally correct.
2. Throw away the parts that are structurally wrong or too costly to evolve.
3. Rebuild in a way that makes comparison against the current branch explicit.

## Rewrite Goal

Rebuild `git-sync` as a smaller set of focused packages around the same product goal:

- remote-to-remote smart HTTP Git mirroring
- empty-target bootstrap relay
- narrow incremental relay
- fallback materialized push path when relay is not safe

## Preserve These Ideas

- Relay-first design is correct.
- Empty-target bootstrap as a distinct strategy is correct.
- Narrow incremental relay instead of trying to make every update streamable is correct.
- Typed result/output structs are useful.
- Machine-readable output is worth keeping.
- Integration-heavy testing is the right testing style for this tool.

## Replace These Structural Choices

- Monolithic orchestration in `internal/syncer/syncer.go`
- Mixed protocol, auth, planning, execution, stats, and measurement concerns in one package
- Partially custom, partially `go-git` transport stack
- Implicit capability handling spread across request builders
- Batch planning that probes by repeatedly fetching full packs and discarding them

## Proposed Rewrite Shape

- `internal/gitproto`
  - pkt-line
  - smart HTTP v1/v2
  - capability negotiation
  - ref advertisement and fetch/push request building
- `internal/planner`
  - desired ref construction
  - mapping normalization and validation
  - plan generation
  - prune policy
  - checkpoint planning
- `internal/auth`
  - credential resolution chain
  - token refresh
  - token store backends
- `internal/strategy/bootstrap`
  - one-shot relay bootstrap
  - batched bootstrap
- `internal/strategy/incremental`
  - incremental relay eligibility and execution
- `internal/strategy/materialized`
  - fetch, local object materialization, and encode/repack push
- `internal/syncer`
  - top-level orchestration only

## Issues

## Correctness

### 1. Batched bootstrap can claim tag refs were pushed when no tag ref was created

Status: done

Problem:
- In the batched bootstrap tag phase, `FetchPack` returning `git.NoErrAlreadyUpToDate` can skip tag creation entirely even when the tag ref is absent and only the tag object is already reachable.
- The result still reports success.

Current code:
- [internal/syncer/syncer.go](/Users/soph/Work/entire/devenv/git-sync/internal/syncer/syncer.go:1308)
- [internal/syncer/syncer.go](/Users/soph/Work/entire/devenv/git-sync/internal/syncer/syncer.go:1328)

Rewrite requirement:
- Ref creation must be independent from pack necessity.
- Support command-only tag creation when objects already exist.

Comparison check:
- A tag whose target object is already present on target must still be created during bootstrap.

Current rewrite note:
- This is now covered directly for the lightweight-tag case where branch batches already made the target object reachable before tag creation.

### 2. Duplicate target mappings are silently accepted

Status: done

Problem:
- Multiple mappings to the same target ref overwrite each other silently.

Current code:
- [internal/syncer/syncer.go](/Users/soph/Work/entire/devenv/git-sync/internal/syncer/syncer.go:1655)

Rewrite requirement:
- Mapping validation must reject duplicate target refs.
- Validation failure must happen before planning or network activity.

Comparison check:
- `--map main:stable --map release:stable` must fail fast with a clear error.

### 3. Mapping normalization accepts inconsistent ref kinds and partially-qualified refs

Status: done

Problem:
- Invalid combinations survive normalization and fail later with misleading errors.

Current code:
- [internal/syncer/syncer.go](/Users/soph/Work/entire/devenv/git-sync/internal/syncer/syncer.go:1706)

Rewrite requirement:
- Normalize short branch names consistently.
- Reject branch-to-tag and tag-to-branch mappings.
- Reject mixed fully-qualified and shorthand forms when ambiguous.

Comparison check:
- Invalid mappings must fail at validation time with precise messages.

### 4. Sideband preference is backwards

Status: done

Problem:
- The current code prefers `sideband` before `sideband64k`.

Current code:
- [internal/syncer/syncer.go](/Users/soph/Work/entire/devenv/git-sync/internal/syncer/syncer.go:3020)

Rewrite requirement:
- Prefer `sideband64k` whenever both are available.

Comparison check:
- Negotiation must pick the highest-capability sideband mode.

### 5. Pack reader leak in bootstrap batch loop

Status: partial

Problem:
- Batch loop fetches a `packReader` without robust close discipline on all error paths.

Current code:
- [internal/syncer/syncer.go](/Users/soph/Work/entire/devenv/git-sync/internal/syncer/syncer.go:1257)

Rewrite requirement:
- Stream lifecycle must be explicit and testable.
- Every fetch stream must have one owner responsible for close.

Comparison check:
- Failing batch pushes must not leak HTTP response bodies.

Current rewrite note:
- Ownership of stream lifecycle is clearer than on `main`, and the rewrite now has direct tests for key pack-stream close behavior on success and error paths.
- Direct strategy-level error-path tests now verify that relay bootstrap and incremental paths close source pack streams when pushes fail.
- Batched integration coverage now also exercises a failed checkpoint pack push followed by a resume-from-temp-ref retry.
- Lower-level `gitproto.PushPack` rejection paths now also close the provided pack stream instead of leaking it on preflight command errors.
- `gitproto.PushPack` now also has direct closure coverage for cancellation, server-side receive-pack errors, and success.
- `gitproto` fetch tests now verify response-body closure symmetry for both v1 and v2 decode-failure paths.
- `fetchToStoreV2` now also has direct cancellation and decode-failure cleanup coverage.
- This still wants a fuller close-audit around lower-level transport interruption paths before it should be considered fully done.

### 6. Protocol v2 tag fetches request `include-tag` without capability gating

Status: done

Problem:
- v2 request building sends `include-tag` without first checking server support.

Current code:
- [internal/syncer/protocol_v2.go](/Users/soph/Work/entire/devenv/git-sync/internal/syncer/protocol_v2.go:639)

Rewrite requirement:
- Capability handling must be centralized and typed.
- Unsupported optional features must never be requested.

Comparison check:
- Tag sync against a v2 server without `include-tag` support must behave correctly.

### 7. OAuth refresh failures are swallowed and stale tokens are reused

Status: done

Problem:
- Token refresh failure degrades into later auth failure with poor diagnostics.

Current code:
- [internal/syncer/syncer.go](/Users/soph/Work/entire/devenv/git-sync/internal/syncer/syncer.go:2775)

Rewrite requirement:
- Distinguish refresh failure from remote auth rejection.
- Surface the actual cause.

Comparison check:
- Expired token plus refresh failure must produce an explicit auth-refresh error path.

## Concurrency And Safety

### 8. `statsCollector` is not safely synchronized

Status: done

Problem:
- The stats map is mutated and read across goroutines without full synchronization.

Current code:
- [internal/syncer/syncer.go](/Users/soph/Work/entire/devenv/git-sync/internal/syncer/syncer.go:3041)

Rewrite requirement:
- Stats collection must be race-free under concurrent HTTP activity.

Comparison check:
- Rewrite branch should pass `go test -race ./...`.

Current rewrite note:
- `go test -race ./...` currently passes on the rewrite branch.

### 9. Unbounded response reads can cause avoidable memory blowups

Status: done

Problem:
- Some response paths use unbounded reads from remote servers.

Current code:
- `protocol_v2.go:713` from review notes

Rewrite requirement:
- Bound responses where protocol shape permits.
- Stream where possible instead of buffering whole bodies.

Comparison check:
- Large or malicious server responses must not be able to force unbounded buffering in normal control paths.

### 10. File token store has no locking

Status: done

Problem:
- Multiple processes can corrupt token storage.

Current code:
- [internal/syncer/syncer.go](/Users/soph/Work/entire/devenv/git-sync/internal/syncer/syncer.go:2931)

Rewrite requirement:
- Add process-safe locking if file store remains supported.
- Or explicitly scope file store to single-process/dev usage and document that.

## Architecture

### 11. `syncer.go` is a monolith

Status: done

Problem:
- One file currently owns protocol setup, planning, batching, auth, stats, and execution.

Current code:
- [internal/syncer/syncer.go](/Users/soph/Work/entire/devenv/git-sync/internal/syncer/syncer.go:397)

Rewrite requirement:
- Package boundaries must isolate protocol, planning, auth, and execution strategies.

Comparison check:
- Top-level orchestrator should be visibly smaller and mostly wiring.

### 12. Entry points duplicate setup work

Status: done

Problem:
- `Run`, `Bootstrap`, `Probe`, and `Fetch` all repeat protocol validation, stats setup, connection setup, and ref discovery.

Rewrite requirement:
- Introduce a shared session/setup layer.

### 13. Functions carry too much ambient state

Status: partial

Problem:
- Large helpers like batched bootstrap depend on too many parameters and too much shared context.

Rewrite requirement:
- Introduce explicit session/context objects with narrow responsibilities.

Current rewrite note:
- Major strategy and protocol concerns were extracted.
- The strategy packages now depend on narrower source-side interfaces instead of the full concrete `gitproto.RefService`.
- The strategies now also depend on a narrower target-side push executor instead of raw target transport state, and direct strategy tests exercise those boundaries.
- Incremental relay policy decisions are now injected consistently instead of splitting between one injected check and one hard-coded planner call.
- Bootstrap checkpoint planning now carries its graph, probe cache, and prefetched-pack state inside a dedicated internal planner object instead of one large helper function.
- Some helpers still carry broad parameter structs, so this remains partial rather than fully complete.

## Performance And Scalability

### 14. Batch planning probes by repeatedly fetching full packs and discarding them

Status: partial

Problem:
- `sourcePackExceedsLimit` can turn checkpoint planning into repeated full-pack fetches.

Current code:
- [internal/syncer/syncer.go](/Users/soph/Work/entire/devenv/git-sync/internal/syncer/syncer.go:1601)

Rewrite requirement:
- Avoid repeated throwaway fetches as the primary sizing mechanism.
- Reuse fitted packs or use a cheaper sizing heuristic.

Comparison check:
- Rewrite branch should significantly reduce HTTP round-trips during batch planning.

Current rewrite note:
- The rewrite adds an initial commit-count heuristic and memoizes repeated equivalent probe results within a planning run.
- Successful under-limit probe packs are now cached and reused during execution, which avoids a second fetch for selected checkpoints.
- The planner still relies on network probes for sizing, so this remains partial rather than fully solved.

### 15. Materialized fallback path does not scale to large repos

Status: partial

Problem:
- The non-relay path stores fetched objects in memory.

Rewrite requirement:
- Decide explicitly whether large non-relay sync is supported.
- If yes, redesign storage strategy.
- If no, fail early and clearly outside safe operating bounds.

Current rewrite note:
- The rewrite introduces a materialized strategy package, exposes an explicit `--materialized-max-objects` operating limit, and fails clearly when that bound is exceeded.
- It still relies on in-memory object storage rather than a fundamentally new scaling model, so this remains partial.

### 16. Fast-forward checks can degenerate into full graph walks

Status: done

Problem:
- `reachesCommitHash` can traverse very large histories.

Current code:
- [internal/syncer/syncer.go](/Users/soph/Work/entire/devenv/git-sync/internal/syncer/syncer.go:2533)

Rewrite requirement:
- Make ancestry checking cost visible and bounded where possible.

Current rewrite note:
- The planner now uses a depth-limited ancestry check and blocks with an explicit reason when the limit is exceeded.

### 17. Packet parsing allocates too aggressively

Status: done

Problem:
- Packet reader allocates per packet and may create unnecessary GC churn.

Current code:
- [internal/syncer/protocol_v2.go](/Users/soph/Work/entire/devenv/git-sync/internal/syncer/protocol_v2.go:75)

Rewrite requirement:
- Reuse buffers where practical.

Current rewrite note:
- `internal/gitproto.PacketReader` now reuses a fixed header buffer and a growable payload buffer, and the rewrite includes packet-reader benchmarks.

## Test Gaps

### 18. Core planning functions are under-tested directly

Status: done

Missing direct tests include:
- `buildDesiredRefs`
- `buildPlans`
- `planRef`
- `normalizeMapping`
- `objectsToPush`
- `collectPushObjects`
- `firstParentChain`
- `autoBatchMaxPackBytes`
- `bootstrapResumeIndex` error path

Rewrite requirement:
- Pure planning and validation logic should be unit-testable without HTTP fixtures.

### 19. Relay eligibility logic is only tested indirectly

Status: done

Missing direct tests include:
- `canIncrementalRelay`
- `canFullTagCreateRelay`
- `relayFallbackReason`

Rewrite requirement:
- Strategy selection must be testable independently from network execution.

Current rewrite note:
- Relay decision functions now have direct planner-level coverage in addition to higher-level execution-path tests.

### 20. Protocol v2 error handling is under-tested

Status: done

Missing test coverage includes:
- malformed pkt-lines
- truncated packet bodies
- missing `version 2`
- malformed `ls-refs`
- unsupported capability combinations

Rewrite requirement:
- Protocol parser behavior must be locked down with explicit failure tests.

### 21. Missing behavioral coverage

Status: partial

Remaining notable gaps:
- harder transport interruption during active pack streaming
- malformed mid-stream fetch responses after valid startup
- broader end-to-end cancellation once response parsing has begun

Current rewrite note:
- Some of these are now covered, including empty source repo, tag force-retarget, duplicate/conflicting mappings, and tag creation when target objects already exist.
- Batched lightweight-tag creation without an extra pack is now covered directly.
- Basic context cancellation coverage now exists for probe, v1 fetch, v2 streaming fetch, and v2 fetch-to-store.
- Batched bootstrap resume mismatch and final-tip cutover paths now have direct integration coverage.
- Batched bootstrap reruns now also cover the "target ref already created, temp ref cleanup still pending" recovery path.
- Injected temp-ref delete failure during batched cutover is now covered end-to-end, including successful recovery on retry.
- Injected checkpoint pack failure after partial batched progress is now covered end-to-end, including successful resume on retry.
- Some harder transport-interruption and malformed mid-stream failure paths still remain.

### 22. No benchmark coverage for the expensive paths

Status: done

Rewrite requirement:
- Add benchmarks for relay path overhead, planning overhead, and fallback graph/object work.

Current rewrite note:
- Planner and protocol benchmarks exist, and the rewrite now also includes syncer-level execution-path benchmarks for bootstrap relay, incremental relay, and a materialized fallback case.

## Rewrite Branch Acceptance Criteria

- All mapping validation happens before network activity. Status: done
- Capability negotiation is centralized and enforced consistently. Status: partial
  Source-side fetch capability checks now live behind `gitproto.RefService` methods, and planner relay gating now consumes a narrower syncer-level policy instead of importing `gitproto` types directly. Some target-side relay decisions still rely on orchestration wiring rather than a fully unified capability model, so this remains partial.
- Relay strategies are separate packages with explicit inputs and outputs. Status: done
- Tag creation is correct whether or not a pack transfer is needed. Status: done
- Stats are concurrency-safe. Status: done
- Logging is structured and concurrency-safe. Status: done
- Protocol parsing has explicit malformed-input tests. Status: done
- Rewrite passes `go test ./...` and `go test -race ./...`. Status: done
- Rewrite includes benchmarks for the critical planning and execution paths. Status: done
- Rewrite branch can be compared against current behavior using the same integration scenarios. Status: done

Notes:
- Capability handling is much better centralized under `internal/gitproto`, but the rewrite still uses `go-git` transport/protocol types rather than fully owning the protocol layer end-to-end.
- Stats are now concurrency-safe and race-tested.
- Bootstrap logging now uses structured `slog` output instead of ad hoc formatted stderr lines.

## Suggested Execution Order

1. Define package boundaries and minimal interfaces.
2. Rebuild mapping validation and planning first.
3. Rebuild protocol and capability layer.
4. Rebuild bootstrap relay.
5. Rebuild incremental relay.
6. Rebuild fallback materialized push path.
7. Rebuild auth and token store behavior.
8. Port and expand tests.
9. Compare rewrite branch behavior against current integration fixtures.
