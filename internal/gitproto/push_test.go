package gitproto

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"
	"github.com/go-git/go-git/v5/plumbing/transport"
)

func TestOpenV2PackStreamCloseClosesBody(t *testing.T) {
	body := &trackingReadCloser{
		ReadCloser: io.NopCloser(bytes.NewBufferString(
			FormatPktLine("packfile\n"),
		)),
	}

	rc, err := openV2PackStream(body)
	if err != nil {
		t.Fatalf("openV2PackStream: %v", err)
	}
	if err := rc.Close(); err != nil {
		t.Fatalf("close pack stream: %v", err)
	}
	if !body.closed {
		t.Fatal("expected underlying body to be closed")
	}
}

func TestPushPackClosesPackOnSuccess(t *testing.T) {
	pack := &trackingReadCloser{ReadCloser: io.NopCloser(bytes.NewBufferString("PACK"))}
	conn := &Conn{
		Transport: fakeTransport{receivePackSession: &fakeReceivePackSession{}},
	}
	adv := packp.NewAdvRefs()
	adv.Capabilities = capability.NewList()

	err := PushPack(context.Background(), conn, adv, []PushCommand{{
		Name: "refs/heads/main",
	}}, pack, false)
	if err != nil {
		t.Fatalf("PushPack returned error: %v", err)
	}
	if !pack.closed {
		t.Fatal("expected pack to be closed on success")
	}
}

func TestPushPackClosesPackOnReceivePackError(t *testing.T) {
	pack := &trackingReadCloser{ReadCloser: io.NopCloser(bytes.NewBufferString("PACK"))}
	conn := &Conn{
		Transport: fakeTransport{receivePackSession: &fakeReceivePackSession{
			err: errors.New("receive-pack failed"),
		}},
	}
	adv := packp.NewAdvRefs()
	adv.Capabilities = capability.NewList()

	err := PushPack(context.Background(), conn, adv, []PushCommand{{
		Name: "refs/heads/main",
	}}, pack, false)
	if err == nil {
		t.Fatal("expected PushPack to return an error")
	}
	if !pack.closed {
		t.Fatal("expected pack to be closed on error")
	}
}

type trackingReadCloser struct {
	io.ReadCloser
	closed bool
}

func (r *trackingReadCloser) Close() error {
	r.closed = true
	if r.ReadCloser != nil {
		return r.ReadCloser.Close()
	}
	return nil
}

type fakeTransport struct {
	receivePackSession transport.ReceivePackSession
}

func (t fakeTransport) NewUploadPackSession(*transport.Endpoint, transport.AuthMethod) (transport.UploadPackSession, error) {
	return nil, errors.New("not implemented")
}

func (t fakeTransport) NewReceivePackSession(*transport.Endpoint, transport.AuthMethod) (transport.ReceivePackSession, error) {
	return t.receivePackSession, nil
}

type fakeReceivePackSession struct {
	err error
}

func (s *fakeReceivePackSession) AdvertisedReferences() (*packp.AdvRefs, error) {
	return packp.NewAdvRefs(), nil
}

func (s *fakeReceivePackSession) AdvertisedReferencesContext(context.Context) (*packp.AdvRefs, error) {
	return s.AdvertisedReferences()
}

func (s *fakeReceivePackSession) ReceivePack(context.Context, *packp.ReferenceUpdateRequest) (*packp.ReportStatus, error) {
	if s.err != nil {
		return nil, s.err
	}
	return nil, nil
}

func (s *fakeReceivePackSession) Close() error { return nil }
