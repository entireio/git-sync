package bootstrap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"

	"entire.io/entire/git-sync/internal/gitproto"
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
	p.logPush(context.Background(), batch, plumbing.ZeroHash, 0, o, 512<<20, false, false, false, false, ErrPackUploadAborted)
	var event map[string]any
	if err := json.Unmarshal(out.Bytes(), &event); err != nil {
		t.Fatal(err)
	}
	if event["abort_reason"] != "projection" || event["projected_pack_bytes"] != float64(800<<20) || event["objects_processed"] != float64(1) {
		t.Fatalf("diagnostic lost the abort decision inputs: %v", event)
	}
}

func TestConfiguredFallbackDiagnostic(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	p := bottomOutParams(t, 64, 0, drainAbort)
	p.FallbackMaxPackBytes = 128
	p.PushLogger = slog.New(slog.NewJSONHandler(&out, nil))
	_, err := Execute(context.Background(), p, "bootstrap-resume-marker")
	if !errors.Is(err, ErrPackUploadAborted) || errors.Is(err, ErrCheckpointExceedsTargetLimit) {
		t.Fatalf("wrong classification: %v", err)
	}
	var event map[string]any
	if err := json.Unmarshal(out.Bytes(), &event); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]any{
		"budget_source": "configured_fallback", "abort_threshold_bytes": float64(128),
		"announced_target_limit_bytes": float64(0), "configured_fallback_limit_bytes": float64(128), "abort_reason": "bytes_read",
	} {
		if event[key] != want {
			t.Errorf("%s=%v, want %v", key, event[key], want)
		}
	}
}

func TestCutUploadDiagnosticDoesNotReportObserverFailure(t *testing.T) {
	t.Parallel()
	data, _ := buildSyntheticPack(t, 10)
	observer := newPackStreamObserver(io.NopCloser(bytes.NewReader(data)))
	observer.SetAborter(func(n, _, _ int64, _ bool) bool { return n >= 64 })
	if _, err := observer.Read(make([]byte, 64)); !errors.Is(err, ErrPackUploadAborted) {
		t.Fatalf("expected abort, got %v", err)
	}
	if err := observer.Close(); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	p := Params{PushLogger: slog.New(slog.NewJSONHandler(&out, nil))}
	batch := plannedBatch{}
	batch.Checkpoints = []plumbing.Hash{plumbing.NewHash("1234567890123456789012345678901234567890")}
	p.logPush(context.Background(), batch, plumbing.ZeroHash, 0, observer, 64, false, true, false, false, ErrPackUploadAborted)
	var event map[string]any
	if err := json.Unmarshal(out.Bytes(), &event); err != nil {
		t.Fatal(err)
	}
	if event["observer_failed"] != false || event["observer_interrupted"] != true || event["aborted_early"] != true {
		t.Fatalf("expected interrupted observation, not instrumentation failure: %v", event)
	}
}

func TestSourceTruncationDiagnosticReportsFailure(t *testing.T) {
	t.Parallel()
	data, _ := buildSyntheticPack(t, 10)
	observer := newPackStreamObserver(io.NopCloser(&sourceFailureReader{data: data[:64], failure: io.ErrUnexpectedEOF}))
	_, pushErr := io.Copy(io.Discard, observer)
	if !errors.Is(pushErr, io.ErrUnexpectedEOF) {
		t.Fatalf("expected source truncation: %v", pushErr)
	}
	if err := observer.Close(); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	p := Params{PushLogger: slog.New(slog.NewJSONHandler(&out, nil))}
	batch := plannedBatch{}
	batch.Checkpoints = []plumbing.Hash{plumbing.NewHash("1234567890123456789012345678901234567890")}
	p.logPush(context.Background(), batch, plumbing.ZeroHash, 0, observer, 1024, false, true, false, false, pushErr)
	var event map[string]any
	if err := json.Unmarshal(out.Bytes(), &event); err != nil {
		t.Fatal(err)
	}
	if event["observer_failed"] != true || event["observer_interrupted"] != false || event["push_failed"] != true {
		t.Fatalf("source truncation mislabeled: %v", event)
	}
}

func TestExecuteSettlesObservationWhenPusherDoesNotClose(t *testing.T) {
	t.Parallel()
	data, _ := buildSyntheticPack(t, 10)
	var out bytes.Buffer
	p := bottomOutParams(t, 64, 0, drainAbort)
	source, ok := p.SourceService.(fakeBootstrapSource)
	if !ok {
		t.Fatal("expected fixture source")
	}
	source.fetchPack = func(context.Context, gitproto.Conn, map[plumbing.ReferenceName]gitproto.DesiredRef, map[plumbing.ReferenceName]plumbing.Hash) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(data)), nil
	}
	p.SourceService = source
	p.PushLogger = slog.New(slog.NewJSONHandler(&out, nil))
	// Return without consuming or closing, as an early server rejection can do.
	p.TargetPusher = fakeBootstrapPusher{pushPack: func(context.Context, []gitproto.PushCommand, io.ReadCloser) error {
		return errors.New("http 413: request body too large")
	}}
	_, err := Execute(context.Background(), p, "bootstrap-resume-marker")
	if err == nil {
		t.Fatal("expected rejection")
	}
	var event map[string]any
	if err := json.Unmarshal(out.Bytes(), &event); err != nil {
		t.Fatal(err)
	}
	if event["observer_interrupted"] != true || event["observer_failed"] != false {
		t.Fatalf("observer not settled: %v", event)
	}
}
