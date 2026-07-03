package gitproto

import (
	"bytes"
	"context"
	"crypto"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/format/packfile"
	"github.com/go-git/go-git/v6/plumbing/hash"
	"github.com/go-git/go-git/v6/plumbing/protocol/capability"
	"github.com/go-git/go-git/v6/plumbing/protocol/packp"
	"github.com/go-git/go-git/v6/plumbing/protocol/packp/sideband"
	"github.com/go-git/go-git/v6/plumbing/storer"
	"github.com/go-git/go-git/v6/plumbing/transport"
)

// PushCommand represents a single ref update command.
type PushCommand struct {
	Name   plumbing.ReferenceName
	Old    plumbing.Hash
	New    plumbing.Hash
	Delete bool
}

// Pusher wraps target-side receive-pack state behind a smaller execution API.
// When OnRejection is non-nil, per-ref ng statuses invoke it instead of erroring;
// pack-level unpack failure remains fatal.
//
// Returned by NewPusher as a pointer so callers can attach OnRejection after
// construction without worrying about whether downstream strategies have
// already captured a value copy.
type Pusher struct {
	Conn        Conn
	Adv         *packp.AdvRefs
	Verbose     bool
	OnRejection func(refName plumbing.ReferenceName, status string)

	// MaxRefUpdates caps ref-update commands per receive-pack request. Zero
	// uses the env-or-default limit (see MaxRefUpdatesEnv); a positive value
	// overrides it — e.g. from the --target-max-ref-updates flag.
	MaxRefUpdates int
}

// NewPusher builds a target-side push executor.
func NewPusher(conn Conn, adv *packp.AdvRefs, verbose bool) *Pusher {
	return &Pusher{Conn: conn, Adv: adv, Verbose: verbose}
}

// defaultMaxRefUpdatesPerPush bounds how many ref-update commands ride in a
// single receive-pack request. The default is deliberately conservative:
// GitHub returns 500 Internal Server Error when a single push updates ~10k refs
// at once but accepts 5k, so 5_000 mirrors a many-ref repo there without
// tripping its (undocumented) ceiling. entire-server tolerates far more — its
// hard cap is 25_000 (server/githttp.maxRefUpdateCommands) — so trusted callers
// pushing to entire-server raise this via MaxRefUpdatesEnv to cut round trips.
//
// Splitting is safe because the pack accompanying the first batch carries every
// object for the whole push: receive-pack commits the entire received pack into
// the object store (entire-server via CommitQuarantinedFanout, canonical git via
// tmp_objdir_migrate — neither prunes objects unreachable from the pushed tips),
// so the remaining batches only need to move ref pointers and carry no pack.
const defaultMaxRefUpdatesPerPush = 5_000

// MaxRefUpdatesEnv overrides defaultMaxRefUpdatesPerPush with a positive
// integer. Raise it for targets known to accept large ref-update pushes (e.g.
// entire-server, up to its 25_000 cap) to reduce round trips; lower it for a
// provider that rejects even the default. Invalid or non-positive values fall
// back to the default.
const MaxRefUpdatesEnv = "GITSYNC_MAX_REF_UPDATES_PER_PUSH"

// maxRefUpdatesPerPush is resolved once from the environment so the limit can be
// tuned per target without rebuilding (see MaxRefUpdatesEnv).
var maxRefUpdatesPerPush = resolveMaxRefUpdatesPerPush()

func resolveMaxRefUpdatesPerPush() int {
	if v := os.Getenv(MaxRefUpdatesEnv); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return defaultMaxRefUpdatesPerPush
}

// effectiveMaxRefUpdates resolves a per-push limit: a positive override wins,
// otherwise the env-or-default limit applies.
func effectiveMaxRefUpdates(maxRefUpdates int) int {
	if maxRefUpdates > 0 {
		return maxRefUpdates
	}
	return maxRefUpdatesPerPush
}

// chunkRefUpdates splits commands into batches no larger than limit. Input that
// already fits is returned as a single batch (including the empty slice, so
// callers preserve their one-request behavior).
func chunkRefUpdates(commands []PushCommand, limit int) [][]PushCommand {
	if len(commands) <= limit {
		return [][]PushCommand{commands}
	}
	batches := make([][]PushCommand, 0, (len(commands)+limit-1)/limit)
	for start := 0; start < len(commands); start += limit {
		end := min(start+limit, len(commands))
		batches = append(batches, commands[start:end])
	}
	return batches
}

// splitFirstBatch peels off the first batch (up to limit) so a push can carry
// the pack with that batch and send the remainder as ref-only follow-ups. rest
// is nil when commands already fit in a single request.
func splitFirstBatch(commands []PushCommand, limit int) (first, rest []PushCommand) {
	if len(commands) <= limit {
		return commands, nil
	}
	return commands[:limit], commands[limit:]
}

// logRefUpdateBatch reports completion of one ref-update batch to the progress
// writer. Ref-only follow-up batches push with progress suppressed (their
// sideband carries nothing but a bare "target:" line per batch), so this is the
// only per-batch signal; it stays quiet unless verbose and the push actually
// spanned multiple batches.
func logRefUpdateBatch(conn Conn, verbose bool, batchNum, totalBatches, refs int) {
	if !verbose || totalBatches <= 1 {
		return
	}
	w := conn.ProgressWriter()
	if w == nil {
		w = os.Stderr
	}
	fmt.Fprintf(w, "target: pushed ref-update batch %d/%d (%d refs)\n", batchNum, totalBatches, refs)
}

// PushPack streams a pack to the target.
func (p *Pusher) PushPack(ctx context.Context, commands []PushCommand, pack io.ReadCloser) error {
	return PushPack(ctx, p.Conn, p.Adv, commands, pack, p.MaxRefUpdates, p.Verbose, p.OnRejection)
}

// PushCommands sends ref-only updates. Creates/updates carry an empty pack;
// delete-only pushes carry no pack. See the package-level PushCommands.
func (p *Pusher) PushCommands(ctx context.Context, commands []PushCommand) error {
	return PushCommands(ctx, p.Conn, p.Adv, commands, p.MaxRefUpdates, p.Verbose, p.OnRejection)
}

// PushObjects encodes and pushes locally materialized objects.
func (p *Pusher) PushObjects(ctx context.Context, commands []PushCommand, store storer.Storer, hashes []plumbing.Hash) error {
	return PushObjects(ctx, p.Conn, p.Adv, commands, store, hashes, p.MaxRefUpdates, p.Verbose, p.OnRejection)
}

// buildUpdateRequest builds the receive-pack update request.
func buildUpdateRequest(
	adv *packp.AdvRefs,
	commands []PushCommand,
	verbose bool,
) (*packp.UpdateRequests, bool, bool, error) {
	req := &packp.UpdateRequests{}
	if sb := PreferredSideband(&adv.Capabilities); sb != "" {
		req.Capabilities.Set(sb)
	}
	if adv.Capabilities.Supports(capability.ReportStatus) {
		req.Capabilities.Set(capability.ReportStatus)
	}

	hasDelete := false
	hasUpdates := false
	for _, cmd := range commands {
		c := &packp.Command{Name: cmd.Name, Old: cmd.Old}
		if cmd.Delete {
			c.New = plumbing.ZeroHash
			hasDelete = true
		} else {
			c.New = cmd.New
			hasUpdates = true
		}
		req.Commands = append(req.Commands, c)
	}

	if hasDelete {
		if !adv.Capabilities.Supports(capability.DeleteRefs) {
			return nil, false, false, errors.New("target does not support delete-refs")
		}
		req.Capabilities.Set(capability.DeleteRefs)
	}

	_ = verbose // progress handling is server-side in HTTP mode
	return req, hasDelete, hasUpdates, nil
}

// leaseFailureMarkers are receive-pack ng reason substrings that indicate the
// captured target tip didn't match what was on the server at push time. Match
// is case-insensitive. CommandStatusErr.Status is a free-form string in go-git,
// so substring matching is the only option absent upstream sentinels.
var leaseFailureMarkers = []string{
	"stale info",
	"fetch first",
	"non-fast-forward",
	"does not match",
}

// IsLeaseFailure reports whether a receive-pack ng reason indicates the
// captured target tip no longer matched at push time. Callers that downgrade
// per-ref rejections to warnings (BestEffort) must still treat these as fatal
// to preserve --force-with-lease semantics.
func IsLeaseFailure(status string) bool {
	lowered := strings.ToLower(status)
	for _, marker := range leaseFailureMarkers {
		if strings.Contains(lowered, marker) {
			return true
		}
	}
	return false
}

// annotateLeaseFailure wraps a lease-failure CommandStatusErr with a retry/
// override hint. Other receive-pack errors pass through unchanged.
func annotateLeaseFailure(err error) error {
	cs, ok := commandStatusErr(err)
	if !ok {
		return err
	}
	if !IsLeaseFailure(cs.Status) {
		return err
	}
	return fmt.Errorf("%w (target ref %s moved or differs from session start; rerun, or use --force-blind to overwrite)", err, cs.ReferenceName)
}

// ErrTargetRefMoved is reported (wrapped) when a push to the target was rejected
// because the target ref changed concurrently between this run's plan and its
// push — a benign, retryable compare-and-swap / lease miss rather than a real
// failure. Test for it with errors.Is(err, ErrTargetRefMoved); the concrete
// error in the chain is a *RefRejectedError. Re-exported publicly as
// gitsync.ErrTargetRefMoved.
var ErrTargetRefMoved = errors.New("target ref moved concurrently")

// RefRejectedError is a single per-ref "ng" status returned by the target's
// receive-pack report-status. Ref is the rejected ref; Reason is the raw,
// server-defined reason text — the git wire protocol carries no structured error
// code, so Reason is free-form and server-specific. Reach it with errors.As.
// Rejections git-sync can prove are concurrent target-ref moves additionally
// satisfy errors.Is(err, ErrTargetRefMoved), letting callers branch on the cause
// without substring-matching Reason themselves. Re-exported publicly as
// gitsync.RefRejectedError.
//
// When a single push rejects multiple refs, report-status surfaces the first
// failing ref (go-git follows canonical git here), so Ref/Reason reflect that
// first ref; any others resurface on the next attempt. The ErrTargetRefMoved
// classification cannot be reproduced by external construction (the deciding
// field is unexported by design) — to exercise errors.Is in a downstream test,
// wrap the sentinel directly: fmt.Errorf("...: %w", gitsync.ErrTargetRefMoved).
type RefRejectedError struct {
	Ref    string // the rejected ref, e.g. "refs/heads/main"
	Reason string // raw receive-pack ng reason, e.g. "remote ref has changed"

	moved bool  // git-sync's mode-independent judgment: an unambiguous concurrent move
	err   error // underlying error; preserves *packp.CommandStatusErr (+ any lease-hint annotation)
}

// Error is safe on a zero-value/externally-constructed RefRejectedError (one
// with no wrapped err), so embedders can build &RefRejectedError{Ref, Reason}
// in tests without a nil panic.
func (e *RefRejectedError) Error() string {
	if e.err == nil {
		return fmt.Sprintf("ref %s rejected: %s", e.Ref, e.Reason)
	}
	return e.err.Error()
}

// Unwrap exposes the underlying receive-pack error so existing
// errors.As(*packp.CommandStatusErr) checks — and substring inspection of the
// message — keep working byte-for-byte for callers that have not migrated.
func (e *RefRejectedError) Unwrap() error { return e.err }

// Is matches ErrTargetRefMoved only when this rejection is a concurrent
// target-ref move. Other rejections remain reachable via errors.As but are not
// ErrTargetRefMoved.
func (e *RefRejectedError) Is(target error) bool {
	return target == ErrTargetRefMoved && e.moved
}

// concurrentMoveMarkers are receive-pack ng reasons that UNAMBIGUOUSLY mean the
// target ref changed under us between plan and push — a clean compare-and-swap /
// lease miss that a plain retry resolves. "Changed" covers both an existing ref
// moving to a new tip AND a planned-absent ref appearing: each is a concurrent
// push of the same repo winning the race, and each self-heals on the next run
// (re-plan against the new state usually no-ops). Deliberately NARROWER than
// leaseFailureMarkers: "non-fast-forward" / "fetch first" are excluded because an
// update that is legitimately non-fast-forward and wasn't force-pushed looks
// identical to a race, and treating it as a benign move would mask a real
// "needs --force" failure.
//
// This set is server-specific by design. "remote ref has changed" is
// entire-server's update-side compare-and-swap rejection
// (storage.ErrReferenceHasChanged): the planned old hash no longer matches the
// target tip. "already exists" is the same CAS on the create side — git-sync
// only emits a create command (old = zero hash) for a ref it found ABSENT at
// plan time, so the server reporting it present at push time can only mean a
// concurrent sync created it first. That makes "already exists" exactly as
// unambiguous a race as "remote ref has changed", just for the create rather
// than the update path; both are the rejections git-sync's own targets emit, and
// the cases that matter in practice. (entire-server's bootstrap-mode planner has
// a separate "target ref ... already exists" *planning* error, but that never
// reaches receive-pack report-status, so it can't surface here.)
//
// "stale info" is git's force-with-lease lease-miss phrasing, kept for
// consistency with leaseFailureMarkers and defence-in-depth; note it is
// primarily a client-side status, so it may not arrive as a server ng reason on
// every target. Stock git servers phrase a CAS miss differently again
// ("failed to update ref" / "cannot lock ref ... but expected ..."), which this
// set does NOT match — so against a non-entire target a genuine race may fall
// through to a plain rejection. Extend this set as new server phrasings are
// observed. Match is case-insensitive substring (Reason is free-form; see
// RefRejectedError).
var concurrentMoveMarkers = []string{
	"remote ref has changed",
	"already exists",
	"stale info",
}

// isConcurrentMove reports whether a receive-pack ng reason is an unambiguous
// concurrent target-ref move (see concurrentMoveMarkers).
func isConcurrentMove(reason string) bool {
	lowered := strings.ToLower(reason)
	for _, marker := range concurrentMoveMarkers {
		if strings.Contains(lowered, marker) {
			return true
		}
	}
	return false
}

// commandStatusErr extracts go-git's per-ref CommandStatusErr from err's chain.
// errors.As is EXACT about value-vs-pointer, and the form go-git uses is not
// part of its API contract: today report.Error() returns the error BY VALUE
// (value receiver Error(), constructed by value in report_status.go), but go-git
// is an alpha dependency and could switch to a *CommandStatusErr. A target that
// only matches one form would silently stop classifying every rejection if the
// other showed up — the exact failure a pointer-only target caused before. So we
// try the value form first (the current reality) and fall back to the pointer
// form. TestAsRefRejectedError_RealReportStatusPath drives go-git's real
// report.Error() so a deeper type change still fails loud in CI.
func commandStatusErr(err error) (packp.CommandStatusErr, bool) {
	var byVal packp.CommandStatusErr
	if errors.As(err, &byVal) {
		return byVal, true
	}
	var byPtr *packp.CommandStatusErr
	if errors.As(err, &byPtr) && byPtr != nil {
		return *byPtr, true
	}
	return packp.CommandStatusErr{}, false
}

// asRefRejectedError wraps a target receive-pack report-status "ng" error in
// a typed *RefRejectedError so callers can branch on errors.As /
// errors.Is(err, ErrTargetRefMoved) instead of substring-matching the free-form
// reason themselves. Inputs that are not a per-ref command status (e.g. an
// unpack-status error) pass through unchanged. The input is preserved via Unwrap,
// so the message and the underlying packp.CommandStatusErr stay reachable.
func asRefRejectedError(err error) error {
	cs, ok := commandStatusErr(err)
	if !ok {
		return err
	}
	return &RefRejectedError{
		Ref:    cs.ReferenceName.String(),
		Reason: cs.Status,
		moved:  isConcurrentMove(cs.Status),
		err:    err,
	}
}

// sendReceivePack encodes and POSTs a receive-pack request, then decodes the report.
func sendReceivePack(
	ctx context.Context,
	conn Conn,
	req *packp.UpdateRequests,
	packData io.Reader,
	verbose bool,
	onRejection func(plumbing.ReferenceName, string),
) error {
	var header bytes.Buffer
	if err := req.Encode(&header); err != nil {
		return fmt.Errorf("encode update-request: %w", err)
	}
	// The push body is io.MultiReader(header, packData); packData comes
	// from a live upload-pack pipe and isn't rewindable, so a mid-stream
	// 401 can't trigger PostRPCStreamBody's normal helper retry. Probe
	// for auth requirements with a same-shape POST first.
	if hc, ok := conn.(*HTTPConn); ok {
		hc.EnsureAuthForService(ctx, transport.ReceivePackService)
	}
	body := io.Reader(bytes.NewReader(header.Bytes()))
	if packData != nil {
		body = io.MultiReader(body, packData)
	}
	reader, err := PostRPCStreamBody(ctx, conn, transport.ReceivePackService, body, false, "receive-pack push")
	if err != nil {
		return fmt.Errorf("target receive-pack: %w", err)
	}
	defer reader.Close()

	// Unwrap sideband if negotiated; stream server-side progress to stderr
	// when verbose so long-running pushes show "Resolving deltas ..." etc.
	var respReader io.Reader = reader
	switch {
	case req.Capabilities.Supports(capability.Sideband64k):
		dem := sideband.NewDemuxer(sideband.Sideband64k, reader)
		dem.Progress = progressSink(verbose, "target: ", conn.ProgressWriter())
		respReader = dem
	case req.Capabilities.Supports(capability.Sideband):
		dem := sideband.NewDemuxer(sideband.Sideband, reader)
		dem.Progress = progressSink(verbose, "target: ", conn.ProgressWriter())
		respReader = dem
	}

	if req.Capabilities.Supports(capability.ReportStatus) {
		report := &packp.ReportStatus{}
		if err := report.Decode(respReader); err != nil {
			return fmt.Errorf("decode report-status: %w", err)
		}
		if onRejection == nil {
			if err := report.Error(); err != nil {
				return fmt.Errorf("report-status: %w", asRefRejectedError(annotateLeaseFailure(err)))
			}
			return nil
		}
		if report.UnpackStatus != "" && report.UnpackStatus != "ok" {
			return fmt.Errorf("report-status: unpack error: %s", report.UnpackStatus)
		}
		for _, cs := range report.CommandStatuses {
			if cs.Status == "" || cs.Status == "ok" {
				continue
			}
			onRejection(cs.ReferenceName, cs.Status)
		}
	}
	return nil
}

// PushObjects pushes locally-materialized objects to the target.
//
// A push within the per-request ref-update limit (see effectiveMaxRefUpdates)
// is a single atomic receive-pack request. A larger push is split: the
// materialized pack — which carries every object for the whole push — rides
// with the first batch of object-bearing commands, then the remaining refs (and
// any deletes) move as ref-only updates because the objects are already
// committed.
func PushObjects(
	ctx context.Context,
	conn Conn,
	adv *packp.AdvRefs,
	commands []PushCommand,
	store storer.Storer,
	hashes []plumbing.Hash,
	maxRefUpdates int,
	verbose bool,
	onRejection func(plumbing.ReferenceName, string),
) error {
	limit := effectiveMaxRefUpdates(maxRefUpdates)
	if len(commands) <= limit {
		return pushObjectsBatch(ctx, conn, adv, commands, store, hashes, verbose, onRejection)
	}

	updates := make([]PushCommand, 0, len(commands))
	var deletes []PushCommand
	for _, c := range commands {
		if c.Delete {
			deletes = append(deletes, c)
		} else {
			updates = append(updates, c)
		}
	}

	if len(updates) > 0 {
		first, rest := splitFirstBatch(updates, limit)
		if err := pushObjectsBatch(ctx, conn, adv, first, store, hashes, verbose, onRejection); err != nil {
			return err
		}
		if len(rest) > 0 {
			if err := PushCommands(ctx, conn, adv, rest, maxRefUpdates, verbose, onRejection); err != nil {
				return err
			}
		}
	}
	if len(deletes) > 0 {
		return PushCommands(ctx, conn, adv, deletes, maxRefUpdates, verbose, onRejection)
	}
	return nil
}

// pushObjectsBatch encodes the selected objects into a pack and sends one
// receive-pack request for commands.
//
// Delta selection runs synchronously up front via
// packfile.DeltaSelector. The selected objects are then handed back to
// a packfile.Encoder behind a passthrough ObjectSelector, so the
// encoder's write phase (Encode → encode(objects)) streams pack bytes
// continuously into an io.Pipe to the HTTP request body. This avoids
// the mid-stream stall that occurs when Encode runs selection itself —
// CDN edges treat the resulting idle gap as a stalled upload and close
// the connection. See go-git PR #2142 for the API hook.
func pushObjectsBatch(
	ctx context.Context,
	conn Conn,
	adv *packp.AdvRefs,
	commands []PushCommand,
	store storer.Storer,
	hashes []plumbing.Hash,
	verbose bool,
	onRejection func(plumbing.ReferenceName, string),
) error {
	req, _, hasUpdates, err := buildUpdateRequest(adv, commands, verbose)
	if err != nil {
		return err
	}
	if !hasUpdates {
		return sendReceivePack(ctx, conn, req, nil, verbose, onRejection)
	}

	progressDest := progressSink(verbose, "target: ", conn.ProgressWriter())

	stopSelect := startSelectionProgress(progressDest)
	objects, err := packfile.NewDeltaSelector(store).ObjectsToPack(hashes, 10)
	stopSelect(len(objects), err)
	if err != nil {
		return fmt.Errorf("select objects to pack: %w", err)
	}

	useRefDeltas := !adv.Capabilities.Supports(capability.OFSDelta)
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		cw := &countingWriter{w: pw}
		stopWrite := startPackWriteProgress(cw, progressDest)
		defer stopWrite()
		enc := packfile.NewEncoder(cw, store, useRefDeltas,
			packfile.WithObjectSelector(precomputedSelector{objects: objects}))
		if _, err := enc.Encode(hashes, 10); err != nil {
			done <- pw.CloseWithError(fmt.Errorf("encode packfile: %w", err))
			return
		}
		done <- pw.Close()
	}()

	err = sendReceivePack(ctx, conn, req, pr, verbose, onRejection)
	_ = pr.Close()
	encodeErr := <-done
	if err != nil {
		return err
	}
	return encodeErr
}

// precomputedSelector is a packfile.ObjectSelector that returns a
// fixed []*packfile.ObjectToPack, ignoring its arguments. It is the
// passthrough used by PushObjects to feed pre-selected objects back
// into packfile.Encoder via WithObjectSelector. Used exactly once per
// PushObjects call and not exposed outside this package.
type precomputedSelector struct {
	objects []*packfile.ObjectToPack
}

func (p precomputedSelector) ObjectsToPack(_ []plumbing.Hash, _ uint) ([]*packfile.ObjectToPack, error) {
	return p.objects, nil
}

// countingWriter wraps an io.Writer and tracks total bytes written.
// The count is read by the progress ticker concurrently with the
// encoder's writes, so the counter is atomic.
type countingWriter struct {
	w io.Writer
	n atomic.Int64
}

func (cw *countingWriter) Write(p []byte) (int, error) {
	n, err := cw.w.Write(p)
	cw.n.Add(int64(n))
	if err != nil {
		return n, fmt.Errorf("counting writer: %w", err)
	}
	return n, nil
}

func (cw *countingWriter) Count() int64 { return cw.n.Load() }

// startSelectionProgress emits in-place "selecting deltas, elapsed X"
// updates every 500ms during the synchronous delta-selection phase of
// PushObjects. The returned stop function takes the number of selected
// objects and the selection error (nil on success); on success it
// finalizes the line with a permanent "selected N objects in Y"
// summary, on error it just stops the ticker without claiming success.
// When dest is nil (non-verbose mode) returns a no-op stop, so
// callers don't need to special-case verbosity.
//
// Selection has no observable byte progress — go-git's DeltaSelector
// is opaque to the caller — so elapsed time is the only signal we can
// surface to keep long selections from looking like a hang.
func startSelectionProgress(dest io.Writer) func(objectCount int, err error) {
	if dest == nil {
		return func(int, error) {}
	}
	start := time.Now()
	ticker := time.NewTicker(500 * time.Millisecond)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				fmt.Fprintf(dest, "selecting deltas, elapsed %s\r",
					time.Since(start).Round(time.Second))
			}
		}
	}()
	return func(objectCount int, err error) {
		ticker.Stop()
		close(stop)
		<-done
		if err != nil {
			return
		}
		fmt.Fprintf(dest, "selected %d objects in %s\n",
			objectCount, time.Since(start).Round(time.Second))
	}
}

// startPackWriteProgress emits in-place "encoding pack: N MB, elapsed
// X" updates every 500ms while the encoder writes pack bytes through
// cw. The returned stop function finalizes the line with a permanent
// "encoded pack" summary. Single-use, typically via defer. When dest
// is nil returns a no-op stop.
//
// This is the second of two phases visible to a materialized push:
// startSelectionProgress runs synchronously first, then
// startPackWriteProgress takes over once selection has completed and
// the encoder begins streaming bytes to the request body.
func startPackWriteProgress(cw *countingWriter, dest io.Writer) func() {
	if dest == nil {
		return func() {}
	}
	start := time.Now()
	ticker := time.NewTicker(500 * time.Millisecond)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				fmt.Fprintf(dest, "encoding pack: %s, elapsed %s\r",
					HumanBytes(cw.Count()), time.Since(start).Round(time.Second))
			}
		}
	}()
	return func() {
		ticker.Stop()
		close(stop)
		<-done
		fmt.Fprintf(dest, "encoded pack: %s in %s\n",
			HumanBytes(cw.Count()), time.Since(start).Round(time.Second))
	}
}

// HumanBytes renders a byte count in IEC-ish binary units (e.g. "47.3 MB"),
// scaling decimal precision down as the value grows. Shared by the progress
// and batching output across syncer, strategy, and gitproto.
func HumanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	value := float64(n) / float64(div)
	suffix := []string{"KB", "MB", "GB", "TB", "PB"}[exp]
	if value >= 100 {
		return fmt.Sprintf("%.0f %s", value, suffix)
	}
	if value >= 10 {
		return fmt.Sprintf("%.1f %s", value, suffix)
	}
	return fmt.Sprintf("%.2f %s", value, suffix)
}

// PushPack pushes a pack stream (relay) to the target.
func PushPack(
	ctx context.Context,
	conn Conn,
	adv *packp.AdvRefs,
	commands []PushCommand,
	pack io.ReadCloser,
	maxRefUpdates int,
	verbose bool,
	onRejection func(plumbing.ReferenceName, string),
) error {
	for _, cmd := range commands {
		if cmd.Delete {
			_ = pack.Close()
			return errors.New("pack push only supports create and update actions")
		}
	}

	// The pack carries every object for all commands, so it rides with the
	// first batch; once committed the remaining refs update without re-sending
	// objects. This keeps each request under the target's per-push ref-update
	// limit (see effectiveMaxRefUpdates).
	first, rest := splitFirstBatch(commands, effectiveMaxRefUpdates(maxRefUpdates))

	req, _, _, err := buildUpdateRequest(adv, first, verbose)
	if err != nil {
		_ = pack.Close()
		return err
	}

	err = sendReceivePack(ctx, conn, req, pack, verbose, onRejection)
	closeErr := pack.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return fmt.Errorf("close pack: %w", closeErr)
	}

	if len(rest) > 0 {
		return PushCommands(ctx, conn, adv, rest, maxRefUpdates, verbose, onRejection)
	}
	return nil
}

// PushCommands sends ref update commands that move no new objects to the
// target — the referenced objects already exist there.
//
// A create/update command still carries a valid empty pack (12-byte header,
// zero objects, trailing checksum). Pack-less creates are legal git, but some
// receive-pack implementations read a pack header for every non-delete command
// and fail with a truncated-pack error when the request body ends after the
// commands; an explicit empty pack satisfies them and stays valid for servers
// that tolerate the pack-less form. Delete-only pushes carry no pack, as git
// requires.
func PushCommands(
	ctx context.Context,
	conn Conn,
	adv *packp.AdvRefs,
	commands []PushCommand,
	maxRefUpdates int,
	verbose bool,
	onRejection func(plumbing.ReferenceName, string),
) error {
	batches := chunkRefUpdates(commands, effectiveMaxRefUpdates(maxRefUpdates))
	for i, batch := range batches {
		// Ref-only batches carry no useful target progress; suppress the empty
		// sideband (verbose=false) and report completion ourselves so a large
		// push doesn't spew a bare "target:" line per batch.
		if err := pushCommandsBatch(ctx, conn, adv, batch, false, onRejection); err != nil {
			return err
		}
		logRefUpdateBatch(conn, verbose, i+1, len(batches), len(batch))
	}
	return nil
}

// pushCommandsBatch sends one receive-pack request for a single batch of
// ref-only commands; the referenced objects must already exist on the target.
func pushCommandsBatch(
	ctx context.Context,
	conn Conn,
	adv *packp.AdvRefs,
	commands []PushCommand,
	verbose bool,
	onRejection func(plumbing.ReferenceName, string),
) error {
	req, _, hasUpdates, err := buildUpdateRequest(adv, commands, verbose)
	if err != nil {
		return err
	}
	var packData io.Reader
	if hasUpdates {
		packData = bytes.NewReader(emptyPack(adv))
	}
	return sendReceivePack(ctx, conn, req, packData, verbose, onRejection)
}

// emptyPackHeader is the fixed 12-byte prefix of any packfile with zero
// objects: the "PACK" signature, version 2, and an object count of 0.
var emptyPackHeader = []byte{'P', 'A', 'C', 'K', 0, 0, 0, 2, 0, 0, 0, 0}

// A valid empty pack is emptyPackHeader followed by the trailing checksum over
// it. The bytes depend only on the hash algorithm, so the two possibilities are
// computed once at package load rather than on every PushCommands call.
var (
	emptyPackSHA1   = buildEmptyPack(crypto.SHA1)
	emptyPackSHA256 = buildEmptyPack(crypto.SHA256)
)

func buildEmptyPack(algo crypto.Hash) []byte {
	h := hash.New(algo)
	_, _ = h.Write(emptyPackHeader)
	return append(slices.Clone(emptyPackHeader), h.Sum(nil)...)
}

// emptyPack returns a valid packfile containing zero objects whose trailing
// checksum matches the target's advertised object format: SHA-256 repositories
// get a 32-byte trailer; everything else uses the 20-byte SHA-1 trailer.
func emptyPack(adv *packp.AdvRefs) []byte {
	if vals := adv.Capabilities.Get(capability.ObjectFormat); len(vals) > 0 && vals[0] == "sha256" {
		return emptyPackSHA256
	}
	return emptyPackSHA1
}

// progressSink returns a line-prefixing io.Writer suitable for
// sideband.Demuxer.Progress. When verbose is false it returns nil so the
// demuxer discards progress frames without allocating. Passing a non-nil
// dest routes the prefixed lines through that writer instead of os.Stderr,
// which lets a live progress reporter coordinate output.
func progressSink(verbose bool, prefix string, dest io.Writer) io.Writer {
	if !verbose {
		return nil
	}
	if dest == nil {
		dest = os.Stderr
	}
	return &prefixedLineWriter{w: dest, prefix: prefix, atLineStart: true}
}

// prefixedLineWriter prepends a fixed prefix to each line of input written
// to the wrapped writer. Git sideband progress arrives as chunks that may
// contain '\n' between full lines or '\r' for in-place updates ("Resolving
// deltas:  12%\r"); both are treated as line terminators so the next chunk
// gets a fresh prefix.
type prefixedLineWriter struct {
	w           io.Writer
	prefix      string
	atLineStart bool
}

func (p *prefixedLineWriter) Write(b []byte) (int, error) {
	consumed := 0
	for len(b) > 0 {
		if p.atLineStart {
			if _, err := io.WriteString(p.w, p.prefix); err != nil {
				return consumed, fmt.Errorf("write prefix: %w", err)
			}
			p.atLineStart = false
		}
		i := bytes.IndexAny(b, "\r\n")
		var chunk []byte
		if i < 0 {
			chunk = b
		} else {
			chunk = b[:i+1]
			p.atLineStart = true
		}
		n, err := p.w.Write(chunk)
		consumed += n
		if err != nil {
			return consumed, fmt.Errorf("write chunk: %w", err)
		}
		b = b[len(chunk):]
	}
	return consumed, nil
}
