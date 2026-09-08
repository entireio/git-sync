package bootstrap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"

	"github.com/go-git/go-git/v6/plumbing"
)

func TestFailedCheckpointEmitsDiagnosticWithoutVerboseLogging(t *testing.T) {
	var out bytes.Buffer
	p := bottomOutParams(t, 64, 0, drainAbort)
	p.PushLogger = slog.New(slog.NewJSONHandler(&out, nil))
	_, err := Execute(context.Background(), p, "bootstrap-resume-marker")
	if !errors.Is(err, ErrPackUploadAborted) {
		t.Fatalf("diagnostics changed upload behavior: %v", err)
	}
	var event map[string]any
	if err := json.Unmarshal(out.Bytes(), &event); err != nil {
		t.Fatalf("expected exactly one diagnostic: %v; output=%s", err, &out)
	}
	for key, want := range map[string]any{
		"effective_budget_bytes": float64(64), "abort_threshold_bytes": float64(60),
		"announced_target_limit_bytes": float64(0), "budget_source": "self_imposed",
		"abort_reason": "bytes_read", "aborted_early": true, "push_failed": true,
		"indivisible": true, "branch": "refs/heads/main",
	} {
		if event[key] != want {
			t.Errorf("%s=%v, want %v", key, event[key], want)
		}
	}
	hash, hashOK := event["checkpoint_hash"].(string)
	bytesRead, bytesOK := event["bytes_read"].(float64)
	if !hashOK || !bytesOK || len(hash) != 40 || bytesRead < 60 {
		t.Fatalf("missing checkpoint or abort counters: %v", event)
	}
}

func TestPushDiagnosticUsesFrozenProjectionInputs(t *testing.T) {
	var out bytes.Buffer
	p := Params{PushLogger: slog.New(slog.NewJSONHandler(&out, nil))}
	o := &packStreamObserver{}
	o.aborted.Store(true)
	o.abortCounters.Store(&packCounters{bytes: 8 << 20, objects: 1, total: 100})
	// Simulate scanner progress after the abort decision. Using this later
	// counter would incorrectly report a tiny projected pack.
	o.objectsSent.Store(100)
	batch := plannedBatch{}
	batch.Checkpoints = []plumbing.Hash{plumbing.NewHash("1234567890123456789012345678901234567890")}
	p.logPush(context.Background(), batch, plumbing.ZeroHash, 0, o, 512<<20, false, false, ErrPackUploadAborted)
	var event map[string]any
	if err := json.Unmarshal(out.Bytes(), &event); err != nil {
		t.Fatal(err)
	}
	if event["abort_reason"] != "projection" || event["projected_pack_bytes"] != float64(800<<20) || event["objects_processed"] != float64(1) {
		t.Fatalf("diagnostic lost the abort decision inputs: %v", event)
	}
}
