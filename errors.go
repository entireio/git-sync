package gitsync

import "entire.io/entire/git-sync/internal/gitproto"

// ErrTargetRefMoved is returned (wrapped) by Sync and Replicate when a push was
// rejected because the target ref changed concurrently between this run's plan
// and its push — a benign, retryable compare-and-swap / lease miss, not a real
// failure. Test for it with errors.Is(err, gitsync.ErrTargetRefMoved). The
// concrete error in the chain is a *RefRejectedError.
//
// This is the supported way to distinguish a racing concurrent push from a
// genuine push failure; prefer it over inspecting the error message text, which
// is free-form and server-specific.
var ErrTargetRefMoved = gitproto.ErrTargetRefMoved

// RefRejectedError is a single per-ref "ng" status returned by the target's
// receive-pack report-status, reachable with errors.As. Ref is the rejected ref
// and Reason is the raw server reason text. Rejections that are concurrent
// target-ref moves also satisfy errors.Is(err, ErrTargetRefMoved).
type RefRejectedError = gitproto.RefRejectedError
