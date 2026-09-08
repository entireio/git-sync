package bootstrap

import (
	"context"
	"log/slog"

	"github.com/go-git/go-git/v6/plumbing"
)

// logPush runs once after PushPack returns, never on each stream Read. For a
// local abort, counters are the exact inputs that triggered the decision; for
// other outcomes they are the observer's latest counters. Neither proves target
// acceptance. In particular, a nil PushPack error can still hide a refused ref.
func (p Params) logPush(ctx context.Context, batch plannedBatch, current plumbing.Hash, idx int, observer *packStreamObserver, budget int64, atAnnounced, fromObservation bool, pushErr error) {
	if p.PushLogger == nil {
		return
	}
	counters := packCounters{observer.Bytes(), observer.ObjectsSent(), observer.TotalObjects()}
	abort := observer.abortCounters.Load()
	if abort != nil {
		counters = *abort
	}
	budgetSource := "self_imposed"
	if fromObservation {
		budgetSource = "observed_cutoff"
	}
	threshold := budget * 95 / 100
	if atAnnounced {
		budgetSource = "target_announced"
		threshold = budget
	}
	projectionAvailable := counters.objects > 0 && counters.total > 0
	var projected float64
	if projectionAvailable {
		projected = float64(counters.bytes) * float64(counters.total) / float64(counters.objects)
	}
	abortReason := "none"
	if abort != nil {
		abortReason = "projection"
		if atAnnounced || counters.bytes >= threshold {
			abortReason = "bytes_read"
		}
	}
	p.PushLogger.InfoContext(ctx, "Bootstrap checkpoint upload diagnostic",
		slog.String("branch", batch.Plan.TargetRef.String()),
		slog.String("checkpoint_hash", batch.Checkpoints[idx].String()),
		slog.String("previous_checkpoint_hash", current.String()),
		slog.Int("checkpoint_index", idx+1),
		slog.Int("checkpoint_count", len(batch.Checkpoints)),
		slog.Bool("indivisible", isIndivisibleCheckpoint(batch, current, idx)),
		slog.Int64("planning_budget_bytes", p.TargetMaxPack),
		slog.Int64("effective_budget_bytes", budget),
		slog.String("budget_source", budgetSource),
		slog.Int64("announced_target_limit_bytes", p.AnnouncedTargetLimit),
		slog.Int64("abort_threshold_bytes", threshold),
		slog.Int64("bytes_read", counters.bytes),
		slog.Int64("objects_processed", counters.objects),
		slog.Int64("total_objects", counters.total),
		slog.Bool("projection_available", projectionAvailable),
		slog.Float64("projected_pack_bytes", projected),
		slog.Bool("aborted_early", observer.Aborted()),
		slog.String("abort_reason", abortReason),
		slog.Bool("observer_failed", observer.ScannerError() != nil),
		slog.Bool("push_failed", pushErr != nil))
}
