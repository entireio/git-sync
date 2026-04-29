# Replicate

`replicate` is a source-authoritative, relay-only operation mode. It is exposed as both a CLI command (`git-sync replicate`) and a stable library entry point (`gitsync.Client.Replicate`), and as a planning mode (`git-sync plan --mode replicate`).

It exists for the case where the operator wants the target to look exactly like the source for the managed ref set, and is willing to overwrite differing target tips to make that true — but is *not* willing to silently materialize the object graph locally if relay cannot be used.

## How it differs from `sync`

`sync` is a reconciliation operation:

- fast-forward checks by default
- `--force` allows non-fast-forward updates
- if relay is not safe (force, prune, tag retarget, or target without `no-thin`), `sync` falls back to a materialized path that decodes objects locally and re-encodes a push pack

`replicate` is an overwrite operation:

- managed target refs that differ are updated to the source tip regardless of fast-forward direction
- there is no materialized fallback
- if relay cannot be used, `replicate` fails and asks the operator to rerun with `sync`

This is intentional. The point of `replicate` is "make target match source via streaming relay or not at all." Falling back to local materialization would defeat that contract.

## Eligibility

`replicate` requires the same relay-eligibility conditions as the incremental relay path inside `sync`:

- target advertises `no-thin` on `receive-pack`. The relayed pack must be self-contained because git-sync's source fetch never requests the `thin-pack` capability — see [protocol.md](protocol.md) for the framing detail.
- the source pack can be streamed directly into target `receive-pack` without local object decoding.

If those conditions are not met, `replicate` aborts before any push.

## Comparison with `sync --force`

`sync --force` and `replicate` overlap but are not the same:

- `sync --force` allows non-fast-forward updates and tag retargeting. It still uses whatever path `sync` chooses, including the materialized fallback.
- `replicate` has the same overwrite intent but pins execution to a relay-only path and refuses to materialize.

If you are running on a target where relay is not available (no `no-thin`, or the protocol shape blocks streaming), `sync --force` will succeed via materialization where `replicate` will fail. That is a feature: it forces the operator to choose between "I want overwrites but I do not want local materialization" (`replicate`) and "I want overwrites and I am fine with local materialization" (`sync --force`).

## When to use `replicate`

Strong fit:

- the operational contract is "target mirrors source," and any divergence on the target is by definition wrong
- the source repository is large enough that local materialization on the runner is undesirable
- the target advertises `no-thin` (most modern Git hosting endpoints and `receive-pack` configurations do)

Weaker fit:

- targets that don't advertise `no-thin`
- workflows that need fast-forward safety on managed refs

## Planning

`git-sync plan --mode replicate` produces the same per-ref action plan as `replicate` would execute, without pushing. Use it before running `replicate` against a non-empty target for the first time.

## Implementation

The execution path lives in `internal/strategy/replicate`. It reuses the relay machinery that `bootstrap` and the incremental relay path inside `sync` use, but with overwrite semantics on the planning side rather than fast-forward checks. Relay framing details (pkt-line, sideband stripping, PACK header handling) are documented in [protocol.md](protocol.md).

## Related

- [bootstrap.md](bootstrap.md) — empty-target relay
- [incremental-relay.md](incremental-relay.md) — narrow relay path inside `sync` for safe updates
- [architecture.md](architecture.md) — operation modes vs transfer modes
