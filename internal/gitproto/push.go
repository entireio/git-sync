package gitproto

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/format/packfile"
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
}

// NewPusher builds a target-side push executor.
func NewPusher(conn Conn, adv *packp.AdvRefs, verbose bool) *Pusher {
	return &Pusher{Conn: conn, Adv: adv, Verbose: verbose}
}

// PushPack streams a pack to the target.
func (p *Pusher) PushPack(ctx context.Context, commands []PushCommand, pack io.ReadCloser) error {
	return PushPack(ctx, p.Conn, p.Adv, commands, pack, p.Verbose, p.OnRejection)
}

// PushCommands sends ref-only updates without a pack.
func (p *Pusher) PushCommands(ctx context.Context, commands []PushCommand) error {
	return PushCommands(ctx, p.Conn, p.Adv, commands, p.Verbose, p.OnRejection)
}

// PushObjects encodes and pushes locally materialized objects.
func (p *Pusher) PushObjects(ctx context.Context, commands []PushCommand, store storer.Storer, hashes []plumbing.Hash) error {
	return PushObjects(ctx, p.Conn, p.Adv, commands, store, hashes, p.Verbose, p.OnRejection)
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
	var cs *packp.CommandStatusErr
	if !errors.As(err, &cs) {
		return err
	}
	if !IsLeaseFailure(cs.Status) {
		return err
	}
	return fmt.Errorf("%w (target ref %s moved or differs from session start; rerun, or use --force-blind to overwrite)", err, cs.ReferenceName)
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
				return fmt.Errorf("report-status: %w", annotateLeaseFailure(err))
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
// Delta selection runs synchronously up front via
// packfile.DeltaSelector. The selected objects are then handed back to
// a packfile.Encoder behind a passthrough ObjectSelector, so the
// encoder's write phase (Encode → encode(objects)) streams pack bytes
// continuously into an io.Pipe to the HTTP request body. This avoids
// the mid-stream stall that occurs when Encode runs selection itself —
// CDN edges treat the resulting idle gap as a stalled upload and close
// the connection. See go-git PR #2142 for the API hook.
func PushObjects(
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
	stopSelect(len(objects))
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
	return n, err
}

func (cw *countingWriter) Count() int64 { return cw.n.Load() }

// startSelectionProgress emits in-place "selecting deltas, elapsed X"
// updates every 500ms during the synchronous delta-selection phase of
// PushObjects. The returned stop function takes the number of selected
// objects and finalizes the line with a permanent "selected N objects
// in Y" summary. When dest is nil (non-verbose mode) returns a no-op
// stop, so callers don't need to special-case verbosity.
//
// Selection has no observable byte progress — go-git's DeltaSelector
// is opaque to the caller — so elapsed time is the only signal we can
// surface to keep long selections from looking like a hang.
func startSelectionProgress(dest io.Writer) func(objectCount int) {
	if dest == nil {
		return func(int) {}
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
	return func(objectCount int) {
		ticker.Stop()
		close(stop)
		<-done
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
					humanizeBytes(cw.Count()), time.Since(start).Round(time.Second))
			}
		}
	}()
	return func() {
		ticker.Stop()
		close(stop)
		<-done
		fmt.Fprintf(dest, "encoded pack: %s in %s\n",
			humanizeBytes(cw.Count()), time.Since(start).Round(time.Second))
	}
}

// humanizeBytes renders n in IEC units with one decimal place for KB+
// (e.g. "47.3 MB"). Anything below 1 KB is shown as raw bytes.
func humanizeBytes(n int64) string {
	const (
		kb = 1024
		mb = kb * 1024
		gb = mb * 1024
	)
	switch {
	case n < kb:
		return fmt.Sprintf("%d B", n)
	case n < mb:
		return fmt.Sprintf("%.1f KB", float64(n)/float64(kb))
	case n < gb:
		return fmt.Sprintf("%.1f MB", float64(n)/float64(mb))
	default:
		return fmt.Sprintf("%.1f GB", float64(n)/float64(gb))
	}
}

// PushPack pushes a pack stream (relay) to the target.
func PushPack(
	ctx context.Context,
	conn Conn,
	adv *packp.AdvRefs,
	commands []PushCommand,
	pack io.ReadCloser,
	verbose bool,
	onRejection func(plumbing.ReferenceName, string),
) error {
	for _, cmd := range commands {
		if cmd.Delete {
			_ = pack.Close()
			return errors.New("pack push only supports create and update actions")
		}
	}

	req, _, _, err := buildUpdateRequest(adv, commands, verbose)
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
	return nil
}

// PushCommands sends ref update commands without a pack (for ref-only changes).
func PushCommands(
	ctx context.Context,
	conn Conn,
	adv *packp.AdvRefs,
	commands []PushCommand,
	verbose bool,
	onRejection func(plumbing.ReferenceName, string),
) error {
	req, _, _, err := buildUpdateRequest(adv, commands, verbose)
	if err != nil {
		return err
	}
	return sendReceivePack(ctx, conn, req, nil, verbose, onRejection)
}

func progressWriter(verbose bool, dest io.Writer) io.Writer {
	if !verbose {
		return nil
	}
	if dest == nil {
		dest = os.Stderr
	}
	return dest
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
