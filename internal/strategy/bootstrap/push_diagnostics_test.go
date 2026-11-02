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
	event := uploadDiagnostic(t, &out)
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
	event := uploadDiagnostic(t, &out)
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
	event := uploadDiagnostic(t, &out)
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
	event := uploadDiagnostic(t, &out)
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
	event := uploadDiagnostic(t, &out)
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
	event := uploadDiagnostic(t, &out)
	if event["observer_interrupted"] != true || event["observer_failed"] != false {
		t.Fatalf("observer not settled: %v", event)
	}
}

// logRecords decodes every JSON log record in out.
func logRecords(t *testing.T, out *bytes.Buffer) []map[string]any {
	t.Helper()
	var records []map[string]any
	dec := json.NewDecoder(bytes.NewReader(out.Bytes()))
	for dec.More() {
		var record map[string]any
		if err := dec.Decode(&record); err != nil {
			t.Fatalf("decode log record: %v; output=%s", err, out)
		}
		records = append(records, record)
	}
	return records
}

// uploadDiagnostic returns the one checkpoint upload record in out, which may
// also carry planning lines.
func uploadDiagnostic(t *testing.T, out *bytes.Buffer) map[string]any {
	t.Helper()
	var found []map[string]any
	for _, record := range logRecords(t, out) {
		if record["msg"] == pushDiagnosticMsg {
			found = append(found, record)
		}
	}
	if len(found) != 1 {
		t.Fatalf("expected exactly one upload diagnostic, got %d; output=%s", len(found), out)
	}
	return found[0]
}

// Planning runs before the first push and can take many minutes on a large
// repo. Its lines must reach PushLogger without -v, or an embedder sees only
// the size lookup and then a timeout.
func TestPlanningLinesReachPushLoggerWithoutVerboseLogging(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	p := bottomOutParams(t, 64, 0, drainAbort)
	p.PushLogger = slog.New(slog.NewJSONHandler(&out, nil))
	if _, err := Execute(context.Background(), p, "bootstrap-resume-marker"); err == nil {
		t.Fatal("expected the bottomed-out upload to fail")
	}
	seen := map[any]bool{}
	for _, record := range logRecords(t, &out) {
		seen[record["msg"]] = true
	}
	for _, msg := range []string{
		"bootstrap batch planning checkpoints",
		"bootstrap batch fetching commit graph",
		"bootstrap batch planned checkpoints",
		"bootstrap batch push attempting",
		"bootstrap batch push stopped at byte budget",
	} {
		if !seen[msg] {
			t.Errorf("PushLogger missing planning line %q; output=%s", msg, &out)
		}
	}
}

// A push git-sync stops itself at its byte budget is how a large bootstrap
// finds its pack size: Info. A push the target refuses is a warning.
func TestPushSetbackLevelFollowsTheCause(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		budget int64
		push   func(int, io.ReadCloser) error
		msg    string
		level  string
	}{
		"stopped at budget": {64, drainAbort, "bootstrap batch push stopped at byte budget", "INFO"},
		"target refused": {1 << 30, func(int, io.ReadCloser) error {
			return errors.New("http 413: body exceeded size limit 1000")
		}, "bootstrap batch push failed", "WARN"},
	} {
		var out bytes.Buffer
		p := bottomOutParams(t, tc.budget, 0, tc.push)
		p.PushLogger = slog.New(slog.NewJSONHandler(&out, nil))
		if _, err := Execute(context.Background(), p, "empty target"); err == nil {
			t.Fatalf("%s: expected the push to fail", name)
		}
		found := false
		for _, record := range logRecords(t, &out) {
			if record["msg"] == tc.msg {
				found = true
				if record["level"] != tc.level {
					t.Errorf("%s: %q logged at %v, want %s", name, tc.msg, record["level"], tc.level)
				}
			}
		}
		if !found {
			t.Errorf("%s: no %q record; output=%s", name, tc.msg, &out)
		}
	}
}

// Under -v a planning line reaches both sinks, and an embedder that hands the
// same logger to both does not get every line twice.
func TestPlanningLineWritesEachLoggerOnce(t *testing.T) {
	t.Parallel()
	var verbose, push bytes.Buffer
	p := Params{
		Logger:     slog.New(slog.NewJSONHandler(&verbose, nil)),
		PushLogger: slog.New(slog.NewJSONHandler(&push, nil)),
	}
	p.log(context.Background(), "bootstrap batch planning checkpoints", "branch_ref_count", 3)
	for name, out := range map[string]*bytes.Buffer{"verbose": &verbose, "push": &push} {
		records := logRecords(t, out)
		if len(records) != 1 || records[0]["branch_ref_count"] != float64(3) {
			t.Errorf("%s log = %v, want the one planning line", name, records)
		}
	}

	var shared bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&shared, nil))
	Params{Logger: logger, PushLogger: logger}.log(context.Background(), "bootstrap batch planning checkpoints")
	if records := logRecords(t, &shared); len(records) != 1 {
		t.Fatalf("shared logger got %d records, want 1", len(records))
	}
}

type ctxKey struct{}

// ctxHandler records the ctxKey value of every record's context, as a handler
// that reads trace IDs from the context would.
type ctxHandler struct {
	slog.Handler

	seen *[]any
}

func (ctxHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h ctxHandler) Handle(ctx context.Context, _ slog.Record) error {
	*h.seen = append(*h.seen, ctx.Value(ctxKey{}))
	return nil
}

// An embedder's handler that reads trace IDs from the context must get the
// caller's context on planning lines, as it does on the upload diagnostic.
func TestPlanningLinesCarryCallerContext(t *testing.T) {
	t.Parallel()
	var seen []any
	p := bottomOutParams(t, 64, 0, drainAbort)
	p.PushLogger = slog.New(ctxHandler{Handler: slog.DiscardHandler, seen: &seen})
	ctx := context.WithValue(context.Background(), ctxKey{}, "trace-1")
	if _, err := Execute(ctx, p, "bootstrap-resume-marker"); err == nil {
		t.Fatal("expected the bottomed-out upload to fail")
	}
	if len(seen) < 2 {
		t.Fatalf("PushLogger got %d records, want planning lines and the upload diagnostic", len(seen))
	}
	for i, v := range seen {
		if v != "trace-1" {
			t.Fatalf("record %d lost the caller's context: got %v", i, v)
		}
	}
}
