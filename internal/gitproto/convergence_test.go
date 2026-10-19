package gitproto

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
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
			p := &Pusher{}
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
			got := p.reconcileRejections(ctx, req, report)
			if read != tc.wantRead {
				t.Errorf("read = %v, want %v", read, tc.wantRead)
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
	progress := strings.Repeat(FormatPktLine("\x02"+strings.Repeat("hook output\n", 2048)), 64)
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
}

func (c *receivePackTestConn) PostRPCStreamBody(context.Context, string, io.Reader, bool, string) (io.ReadCloser, error) {
	return c.response, nil
}

type receivePackTestResponse struct {
	io.Reader

	closeErr error
	closes   int
}

func (r *receivePackTestResponse) Close() error { r.closes++; return r.closeErr }

type receivePackReadError struct{ err error }

func (r receivePackReadError) Read([]byte) (int, error) { return 0, r.err }

func TestPusherPreservesReceivePackCompletionErrors(t *testing.T) {
	for _, phase := range []string{"read", "close"} {
		t.Run(phase, func(t *testing.T) {
			failure := fmt.Errorf("target %s failure", phase)
			report := FormatPktLine("unpack ok\n") + FormatPktLine("ng refs/heads/main stale info\n") + "0000"
			response := &receivePackTestResponse{Reader: strings.NewReader(report)}
			if phase == "close" {
				response.closeErr = failure
			} else {
				response.Reader = io.MultiReader(response.Reader, receivePackReadError{failure})
			}
			adv := &packp.AdvRefs{}
			adv.Capabilities.Set(capability.ReportStatus)
			p := NewPusher(&receivePackTestConn{response: response}, adv, false)
			p.TargetRefsNow = func(context.Context) (map[plumbing.ReferenceName]plumbing.Hash, error) {
				t.Error("must not verify after transport failure")
				return nil, failure
			}
			err := p.PushCommands(t.Context(), []PushCommand{{Name: "refs/heads/main", New: plumbing.NewHash("1111111111111111111111111111111111111111")}})
			if !errors.Is(err, failure) || !errors.Is(err, ErrTargetRefMoved) {
				t.Fatalf("lost transport or ref rejection: %v", err)
			}
			if !strings.Contains(err.Error(), "target receive-pack") || strings.Contains(err.Error(), "close pack reader") {
				t.Fatalf("misleading error: %v", err)
			}
			if response.closes != 1 {
				t.Fatalf("closed %d times", response.closes)
			}
		})
	}
}
