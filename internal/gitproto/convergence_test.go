package gitproto

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/protocol/capability"
	"github.com/go-git/go-git/v6/plumbing/protocol/packp"
)

func TestReconcileConcurrentRefRejections(t *testing.T) {
	main := plumbing.ReferenceName("refs/heads/main")
	other := plumbing.ReferenceName("refs/heads/other")
	wanted := plumbing.NewHash("1111111111111111111111111111111111111111")
	different := plumbing.NewHash("2222222222222222222222222222222222222222")
	for _, tc := range []struct {
		name        string
		status      string
		current     plumbing.Hash
		readErr     error
		cancelRead  bool
		deletion    bool
		unpack      string
		otherError  bool
		bestEffort  bool
		wantRead    bool
		wantCurrent bool
	}{
		{name: "same_hash", status: "remote ref has changed", current: wanted, wantRead: true, wantCurrent: true},
		{name: "git_receive_pack_same_hash", status: "failed to update ref", current: wanted, wantRead: true, wantCurrent: true},
		{name: "git_receive_pack_different_hash", status: "failed to update ref", current: different, wantRead: true},
		{name: "modern_git_same_hash", status: "incorrect old value provided", current: wanted, wantRead: true, wantCurrent: true},
		{name: "modern_git_different_hash", status: "incorrect old value provided", current: different, wantRead: true},
		{name: "same_create", status: "already exists", current: wanted, wantRead: true, wantCurrent: true},
		{name: "different_hash", status: "stale info", current: different, wantRead: true},
		{name: "absent_or_hidden", status: "stale info", wantRead: true},
		{name: "read_failure", status: "stale info", readErr: errors.New("offline"), wantRead: true},
		{name: "canceled_read", status: "stale info", current: wanted, cancelRead: true, wantRead: true},
		{name: "policy_rejection", status: "protected branch", current: wanted},
		{name: "ambiguous_non_fast_forward", status: "non-fast-forward", current: wanted},
		{name: "unconfirmed_delete", status: "stale info", deletion: true},
		{name: "unpack_failure", status: "stale info", current: wanted, unpack: "bad pack"},
		{name: "mixed_rejections_strict", status: "stale info", current: wanted, otherError: true},
		{name: "mixed_rejections_best_effort", bestEffort: true, status: "stale info", current: wanted, otherError: true, wantRead: true, wantCurrent: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var warnings bytes.Buffer
			p := &Pusher{Conn: &receivePackTestConn{progress: &warnings}}
			if tc.bestEffort {
				p.OnRejection = func(plumbing.ReferenceName, string) {}
			}
			read := false
			p.TargetRefsNow = func(context.Context) (map[plumbing.ReferenceName]plumbing.Hash, error) {
				read = true
				if tc.cancelRead {
					cancel()
				}
				return map[plumbing.ReferenceName]plumbing.Hash{main: tc.current, other: wanted}, tc.readErr
			}
			var current []plumbing.ReferenceName
			p.OnAlreadyCurrent = func(name plumbing.ReferenceName, hash plumbing.Hash) {
				if hash != wanted {
					t.Errorf("observed hash = %s, want %s", hash, wanted)
				}
				current = append(current, name)
			}
			newHash := wanted
			if tc.deletion {
				newHash = plumbing.ZeroHash
			}
			req := &packp.UpdateRequests{Commands: []*packp.Command{
				{Name: main, New: newHash}, {Name: other, New: wanted},
			}}
			report := &packp.ReportStatus{UnpackStatus: "ok", CommandStatuses: []*packp.CommandStatus{
				{ReferenceName: main, Status: tc.status},
			}}
			if tc.unpack != "" {
				report.UnpackStatus = tc.unpack
			}
			if tc.otherError {
				report.CommandStatuses = append(report.CommandStatuses, &packp.CommandStatus{ReferenceName: other, Status: "protected branch"})
			}
			got := p.reconcileRejections(ctx, req, report, nil)
			if read != tc.wantRead {
				t.Errorf("read = %v, want %v", read, tc.wantRead)
			}
			if warned := strings.Contains(warnings.String(), "could not check"); warned != (tc.readErr != nil) {
				t.Errorf("warning = %q, want one only for a failed read", warnings.String())
			}
			outcome, _ := p.LastOutcome(main)
			if (outcome == RefOutcomeAlreadyCurrent) != tc.wantCurrent || (len(current) == 1) != tc.wantCurrent {
				t.Errorf("unexpected convergence: outcome=%v callbacks=%v", outcome, current)
			}
			wantError := !tc.wantCurrent || tc.otherError
			if (got.Error() != nil) != wantError {
				t.Errorf("report error = %v, wantError = %v", got.Error(), wantError)
			}
			if tc.otherError && got.CommandStatuses[len(got.CommandStatuses)-1].Status != "protected branch" {
				t.Errorf("unrelated refusal was lost: %+v", got.CommandStatuses)
			}
			if report.CommandStatuses[0].Status != tc.status {
				t.Fatal("original report was changed")
			}
		})
	}
}

// A matching rejected ref must not turn an unattempted later batch into success.
func TestPusherAlreadyCurrentContinuesLaterBatches(t *testing.T) {
	for _, bestEffort := range []bool{false, true} {
		t.Run(map[bool]string{false: "strict", true: "best_effort"}[bestEffort], func(t *testing.T) {
			main := plumbing.ReferenceName("refs/heads/main")
			other := plumbing.ReferenceName("refs/heads/other")
			wanted := plumbing.NewHash("1111111111111111111111111111111111111111")
			var mu sync.Mutex
			deny := main
			server := refReportServer(t, &mu, &deny, "stale info")
			t.Cleanup(server.Close)
			adv := &packp.AdvRefs{}
			adv.Capabilities.Set(capability.ReportStatus)
			p := NewPusher(connForServer(t, server), adv, false)
			p.MaxRefUpdates = 1
			p.TargetRefsNow = func(context.Context) (map[plumbing.ReferenceName]plumbing.Hash, error) {
				return map[plumbing.ReferenceName]plumbing.Hash{main: wanted}, nil
			}
			if bestEffort {
				p.OnRejection = func(name plumbing.ReferenceName, status string) {
					t.Errorf("unexpected rejection: %s %s", name, status)
				}
			}
			if err := p.PushCommands(t.Context(), []PushCommand{{Name: main, New: wanted}, {Name: other, New: wanted}}); err != nil {
				t.Fatal(err)
			}
			if outcome, _ := p.LastOutcome(main); outcome != RefOutcomeAlreadyCurrent {
				t.Errorf("first ref outcome = %v, want already current", outcome)
			}
			if outcome, _ := p.LastOutcome(other); outcome != RefOutcomeApplied {
				t.Errorf("second batch outcome = %v, want applied", outcome)
			}
		})
	}
}

// The inner report ends before sideband progress and the process itself do.
// Enough trailing output to fill a pipe makes an early close break the writer.
func lateReceivePackResponse() string {
	report := FormatPktLine("unpack ok\n") + FormatPktLine("ng refs/heads/main stale info\n") + "0000"
	progress := strings.Repeat(FormatPktLine("\x02"+strings.Repeat("hook output\n", 2048)), 16)
	return FormatPktLine("\x01"+report) + progress + FormatPktLine("\x02hook finished\n") + "0000"
}

func TestPusherDrainsReceivePackBeforeConvergence(t *testing.T) {
	for _, kind := range []string{"ssh", "helper"} {
		t.Run(kind, func(t *testing.T) {
			var conn Conn
			if kind == "helper" {
				conn = fakeHelperConn(t, "late-rejection")
			} else {
				dir := t.TempDir()
				response := filepath.Join(dir, "response")
				if err := os.WriteFile(response, []byte(lateReceivePackResponse()), 0o600); err != nil {
					t.Fatal(err)
				}
				advertisement := filepath.Join(dir, "advertisement")
				if err := os.WriteFile(advertisement, []byte(fakeV0Advertisement), 0o600); err != nil {
					t.Fatal(err)
				}
				script := filepath.Join(dir, "ssh")
				writeShellStub(t, script, "cat "+shellQuote(advertisement)+"\ncat >/dev/null\ncat "+shellQuote(response))
				conn = newSSHTestConn(t, "ssh://example.com/repo.git", script)
			}
			var progress bytes.Buffer
			conn.SetProgressWriter(&progress)
			adv := &packp.AdvRefs{}
			adv.Capabilities.Set(capability.ReportStatus)
			adv.Capabilities.Set(capability.Sideband64k)
			p := NewPusher(conn, adv, true)
			wanted := plumbing.NewHash("1111111111111111111111111111111111111111")
			read := false
			p.TargetRefsNow = func(context.Context) (map[plumbing.ReferenceName]plumbing.Hash, error) {
				read = true
				if !strings.Contains(progress.String(), "hook finished") {
					t.Error("verification started before hook output drained")
					return nil, errors.New("verification started before hook output drained")
				}
				return map[plumbing.ReferenceName]plumbing.Hash{"refs/heads/main": wanted}, nil
			}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			if err := p.PushPack(ctx, []PushCommand{{Name: "refs/heads/main", New: wanted}}, io.NopCloser(bytes.NewReader(emptyPack(adv)))); err != nil {
				t.Fatal(err)
			}
			if !read {
				t.Fatal("convergence was not checked")
			}
			if outcome, _ := p.LastOutcome("refs/heads/main"); outcome != RefOutcomeAlreadyCurrent {
				t.Fatalf("outcome = %v", outcome)
			}
		})
	}
}

type receivePackTestConn struct {
	stubConn

	response io.ReadCloser
	progress io.Writer
}

func (c *receivePackTestConn) PostRPCStreamBody(context.Context, string, io.Reader, bool, string) (io.ReadCloser, error) {
	return c.response, nil
}

func (c *receivePackTestConn) ProgressWriter() io.Writer { return c.progress }

// The push response is drained only before a verification read. Otherwise the
// report is authoritative: teardown errors are ignored as they always were, and
// an early refusal closes at once instead of waiting for the peer to finish.
func TestPusherReceivePackCompletion(t *testing.T) {
	main := plumbing.ReferenceName("refs/heads/main")
	wanted := plumbing.NewHash("1111111111111111111111111111111111111111")
	failure := errors.New("target transport failure")
	accepted := FormatPktLine("unpack ok\n") + FormatPktLine("ok refs/heads/main\n") + "0000"
	rejected := FormatPktLine("unpack ok\n") + FormatPktLine("ng refs/heads/main stale info\n") + "0000"
	tooLarge := FormatPktLine("unpack push rejected: body exceeded size limit 10\n") + FormatPktLine("ng refs/heads/main unpacker error\n") + "0000"
	for _, tc := range []struct {
		name        string
		report      string
		tail        io.Reader
		closeErr    error
		bestEffort  bool
		wantVerify  bool
		wantErr     string
		wantMoved   bool
		wantWarning string
		wantOutcome RefOutcome
	}{
		{name: "accepted_tail_error", report: accepted, tail: iotest.ErrReader(failure), wantOutcome: RefOutcomeApplied},
		{name: "accepted_close_error", report: accepted, closeErr: failure, wantOutcome: RefOutcomeApplied},
		{name: "unpack_failure_not_drained", report: tooLarge, tail: iotest.ErrReader(failure), wantErr: "body exceeded size limit 10"},
		{name: "rejected_tail_error", report: rejected, tail: iotest.ErrReader(failure), wantMoved: true, wantWarning: failure.Error()},
		{name: "rejected_close_error", report: rejected, closeErr: failure, wantMoved: true, wantWarning: failure.Error()},
		{name: "rejected_close_error_best_effort", report: rejected, closeErr: failure, bestEffort: true, wantWarning: failure.Error(), wantOutcome: RefOutcomeRefused},
		{name: "rejected_tail_at_limit", report: rejected, tail: strings.NewReader(strings.Repeat("x", maxDrainBytes)), wantVerify: true, wantOutcome: RefOutcomeAlreadyCurrent},
		{name: "rejected_oversized_tail", report: rejected, tail: strings.NewReader(strings.Repeat("x", maxDrainBytes+1)), wantMoved: true, wantWarning: "bytes after report-status"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var body io.Reader = strings.NewReader(tc.report)
			if tc.tail != nil {
				body = io.MultiReader(body, tc.tail)
			}
			response := &countingCloser{Reader: body, closeErr: tc.closeErr}
			var progress bytes.Buffer
			adv := &packp.AdvRefs{}
			adv.Capabilities.Set(capability.ReportStatus)
			p := NewPusher(&receivePackTestConn{response: response, progress: &progress}, adv, false)
			if tc.bestEffort {
				p.OnRejection = func(plumbing.ReferenceName, string) {}
			}
			p.TargetRefsNow = func(context.Context) (map[plumbing.ReferenceName]plumbing.Hash, error) {
				if !tc.wantVerify {
					t.Error("must not verify after an unclean finish")
					return nil, failure
				}
				return map[plumbing.ReferenceName]plumbing.Hash{main: wanted}, nil
			}
			err := p.PushCommands(t.Context(), []PushCommand{{Name: main, New: wanted}})
			switch {
			case tc.wantErr != "":
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) || errors.Is(err, failure) {
					t.Fatalf("err = %v, want only %q", err, tc.wantErr)
				}
			case tc.wantMoved:
				if !errors.Is(err, ErrTargetRefMoved) || errors.Is(err, failure) {
					t.Fatalf("err = %v, want only the ref rejection", err)
				}
			case err != nil:
				t.Fatalf("push failed: %v", err)
			}
			if outcome, _ := p.LastOutcome(main); outcome != tc.wantOutcome {
				t.Errorf("outcome = %v, want %v", outcome, tc.wantOutcome)
			}
			if got := progress.String(); (tc.wantWarning == "") != (got == "") || !strings.Contains(got, tc.wantWarning) {
				t.Errorf("warning = %q, want %q", got, tc.wantWarning)
			}
			if response.closes != 1 {
				t.Fatalf("closed %d times", response.closes)
			}
		})
	}
}
