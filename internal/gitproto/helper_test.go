package gitproto

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestHelperRemoteHelperProcess is not a real test: it is re-executed as a fake
// git remote helper by the tests below (the standard os/exec helper-process
// pattern). It speaks just enough of the stateless-connect protocol to exercise
// HelperConn. Behaviour is selected via the GITSYNC_FAKE_HELPER_MODE env var.
func TestHelperRemoteHelperProcess(_ *testing.T) {
	if os.Getenv("GITSYNC_FAKE_HELPER") != "1" {
		return
	}
	if err := runFakeHelper(os.Getenv("GITSYNC_FAKE_HELPER_MODE"), os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "fake helper:", err)
		os.Exit(1)
	}
	os.Exit(0)
}

// runFakeHelper emulates git-remote-entire's protocol handling closely enough
// to drive HelperConn: it answers the capabilities probe, then (for a
// stateless-connect-capable advertisement) reads the stateless-connect command,
// writes the empty-line ack, emits a canned advertisement, and services one
// request. upload-pack frames the response with a trailing 0002 (v2 stateless);
// receive-pack streams the response and exits (v0 connect). The "no-stateless"
// mode advertises only `connect` so HelperConn's capability probe rejects it.
func runFakeHelper(mode string, stdin io.Reader, stdout io.Writer) error {
	write := func(s string) error {
		_, err := io.WriteString(stdout, s)
		return err
	}

	br := bufio.NewReader(stdin)

	if line, err := br.ReadString('\n'); err != nil {
		return fmt.Errorf("read capabilities command: %w", err)
	} else if got := strings.TrimRight(line, "\r\n"); got != "capabilities" {
		return fmt.Errorf("expected capabilities, got %q", got)
	}
	if mode == "no-stateless" {
		return write("connect\noption\n\n") // no stateless-connect: probe must reject
	}
	if err := write("stateless-connect\npush\noption\n\n"); err != nil {
		return err
	}

	cmd, err := br.ReadString('\n')
	if err != nil {
		return fmt.Errorf("read command: %w", err)
	}
	cmd = strings.TrimRight(cmd, "\r\n")
	service := strings.TrimPrefix(cmd, "stateless-connect ")

	if mode == "fallback" {
		return write("fallback\n")
	}

	if err := write("\n"); err != nil { // ack: empty line = connection established
		return err
	}

	switch service {
	case "git-upload-pack":
		if err := write(fakeV2Advertisement); err != nil {
			return err
		}
		// Read the client's flush-terminated request. A bare info/refs caller
		// (RequestInfoRefs) sends none and closes stdin — EOF here is a clean
		// end, exactly as the real helper's request loop treats it.
		if _, err := readAdvertisement(br); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return nil
			}
			return fmt.Errorf("read request: %w", err)
		}
		return write(fakeFetchResponse + "0002")
	case "git-receive-pack":
		if err := write(fakeV0Advertisement); err != nil {
			return err
		}
		// Drain the push request (commands + flush + pack-until-EOF), then
		// stream a report-status response and exit.
		if _, err := io.Copy(io.Discard, br); err != nil {
			return fmt.Errorf("drain request: %w", err)
		}
		return write(fakeReportStatus)
	default:
		return write("fallback\n")
	}
}

var (
	// A minimal but well-formed v2 capability advertisement.
	fakeV2Advertisement = FormatPktLine("version 2\n") +
		FormatPktLine("agent=fake/1\n") +
		FormatPktLine("ls-refs=unborn\n") +
		FormatPktLine("fetch=shallow\n") +
		"0000"
	fakeFetchResponse   = FormatPktLine("packfile\n") + "0000"
	fakeV0Advertisement = FormatPktLine("0000000000000000000000000000000000000000 capabilities^{}\x00report-status delete-refs\n") + "0000"
	fakeReportStatus    = FormatPktLine("unpack ok\n") + FormatPktLine("ok refs/heads/main\n") + "0000"
)

// fakeHelperConn builds a HelperConn whose "helper binary" is a tiny wrapper
// script that re-executes this test process as the fake helper (the standard
// os/exec helper-process pattern). The fake calls os.Exit before the testing
// framework can print its summary, so stdout carries only protocol bytes.
func fakeHelperConn(t *testing.T, mode string) *HelperConn {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test binary: %v", err)
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "git-remote-entire")
	content := fmt.Sprintf("#!/bin/sh\n"+
		"export GITSYNC_FAKE_HELPER=1\n"+
		"export GITSYNC_FAKE_HELPER_MODE=%q\n"+
		"exec %q -test.run='^TestHelperRemoteHelperProcess$'\n", mode, exe)
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatalf("write fake helper script: %v", err)
	}

	prev := RemoteHelperLookPath
	RemoteHelperLookPath = func(_ string) (string, bool) { return script, true }
	t.Cleanup(func() { RemoteHelperLookPath = prev })

	path, ok := LookupRemoteHelper("entire")
	if !ok {
		t.Fatal("expected fake helper to resolve")
	}
	ep, err := url.Parse("entire://example.test/repo")
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	return NewHelperConn(path, ep, "entire://example.test/repo", "target")
}

func TestHelperConn_RequestInfoRefs_V2Advertisement(t *testing.T) {
	c := fakeHelperConn(t, "")
	adv, err := c.RequestInfoRefs(context.Background(), "git-upload-pack", GitProtocolV2)
	if err != nil {
		t.Fatalf("RequestInfoRefs: %v", err)
	}
	caps, err := DecodeV2Capabilities(strings.NewReader(string(adv)))
	if err != nil {
		t.Fatalf("DecodeV2Capabilities: %v (adv=%q)", err, adv)
	}
	if !caps.Supports("ls-refs") || !caps.Supports("fetch") {
		t.Fatalf("expected ls-refs and fetch capabilities, got %+v", caps.Caps)
	}
}

func TestHelperConn_PostRPC_V2_StripsResponseEnd(t *testing.T) {
	c := fakeHelperConn(t, "")
	rc, err := c.PostRPCStreamBody(context.Background(), "git-upload-pack",
		strings.NewReader(FormatPktLine("command=fetch\n")+"0000"), true, "fetch")
	if err != nil {
		t.Fatalf("PostRPCStreamBody: %v", err)
	}
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if err := rc.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if string(got) != fakeFetchResponse {
		t.Fatalf("response = %q, want %q (the trailing 0002 must be stripped)", got, fakeFetchResponse)
	}
}

func TestHelperConn_PostRPC_ReceivePack_ReadsToEOF(t *testing.T) {
	c := fakeHelperConn(t, "")
	body := FormatPktLine("0000000000000000000000000000000000000000 1111111111111111111111111111111111111111 refs/heads/main\n") + "0000"
	rc, err := c.PostRPCStreamBody(context.Background(), "git-receive-pack",
		strings.NewReader(body), false, "push")
	if err != nil {
		t.Fatalf("PostRPCStreamBody: %v", err)
	}
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if err := rc.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if string(got) != fakeReportStatus {
		t.Fatalf("response = %q, want %q", got, fakeReportStatus)
	}
}

func TestHelperConn_Fallback(t *testing.T) {
	c := fakeHelperConn(t, "fallback")
	_, err := c.RequestInfoRefs(context.Background(), "git-upload-pack", GitProtocolV2)
	if err == nil {
		t.Fatal("expected error on fallback response")
	}
	if !strings.Contains(err.Error(), "fallback") {
		t.Fatalf("error = %v, want it to mention fallback", err)
	}
}

func TestHelperConn_NoStatelessConnect_MeaningfulError(t *testing.T) {
	c := fakeHelperConn(t, "no-stateless")
	_, err := c.RequestInfoRefs(context.Background(), "git-upload-pack", GitProtocolV2)
	if err == nil {
		t.Fatal("expected error when helper lacks stateless-connect")
	}
	if !strings.Contains(err.Error(), "stateless-connect") {
		t.Fatalf("error = %v, want it to explain the missing stateless-connect capability", err)
	}
}

func TestReadAdvertisement_StopsAtFlush(t *testing.T) {
	in := FormatPktLine("version 2\n") + FormatPktLine("agent=x\n") + "0000" + "extra-bytes"
	br := bufio.NewReader(strings.NewReader(in))
	adv, err := readAdvertisement(br)
	if err != nil {
		t.Fatalf("readAdvertisement: %v", err)
	}
	want := FormatPktLine("version 2\n") + FormatPktLine("agent=x\n") + "0000"
	if string(adv) != want {
		t.Fatalf("adv = %q, want %q", adv, want)
	}
	rest, err := io.ReadAll(br)
	if err != nil {
		t.Fatalf("read leftover: %v", err)
	}
	if string(rest) != "extra-bytes" {
		t.Fatalf("leftover = %q, want the bytes after the flush to remain buffered", rest)
	}
}

func TestResponseEndReader_StripsResponseEnd(t *testing.T) {
	in := FormatPktLine("data-one\n") + "0000" + FormatPktLine("data-two\n") + "0000" + "0002" + "trailing"
	r := newResponseEndReader(bufio.NewReader(strings.NewReader(in)))
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	want := FormatPktLine("data-one\n") + "0000" + FormatPktLine("data-two\n") + "0000"
	if string(got) != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// Guard: building the exec.Cmd for the fake helper goes through the same dial
// path; ensure exec.LookPath agrees the resolved binary is runnable.
func TestRemoteHelperLookPath_Default(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	if _, ok := LookupRemoteHelper(""); ok {
		t.Fatal("empty scheme must not resolve")
	}
}
