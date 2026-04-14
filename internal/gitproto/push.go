package gitproto

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/format/packfile"
	"github.com/go-git/go-git/v6/plumbing/protocol/packp"
	"github.com/go-git/go-git/v6/plumbing/protocol/packp/capability"
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
type Pusher struct {
	Conn    *Conn
	Adv     *packp.AdvRefs
	Verbose bool
}

// NewPusher builds a target-side push executor.
func NewPusher(conn *Conn, adv *packp.AdvRefs, verbose bool) Pusher {
	return Pusher{Conn: conn, Adv: adv, Verbose: verbose}
}

// PushPack streams a pack to the target.
func (p Pusher) PushPack(ctx context.Context, commands []PushCommand, pack io.ReadCloser) error {
	return PushPack(ctx, p.Conn, p.Adv, commands, pack, p.Verbose)
}

// PushCommands sends ref-only updates without a pack.
func (p Pusher) PushCommands(ctx context.Context, commands []PushCommand) error {
	return PushCommands(ctx, p.Conn, p.Adv, commands, p.Verbose)
}

// PushObjects encodes and pushes locally materialized objects.
func (p Pusher) PushObjects(ctx context.Context, commands []PushCommand, store storer.Storer, hashes []plumbing.Hash) error {
	return PushObjects(ctx, p.Conn, p.Adv, commands, store, hashes, p.Verbose)
}

// buildUpdateRequest builds the receive-pack update request.
func buildUpdateRequest(
	adv *packp.AdvRefs,
	commands []PushCommand,
	verbose bool,
) (*packp.UpdateRequests, bool, bool, error) {
	req := packp.NewUpdateRequests()
	if sb := PreferredSideband(adv.Capabilities); sb != "" {
		_ = req.Capabilities.Set(sb)
	}
	if adv.Capabilities.Supports(capability.ReportStatus) {
		_ = req.Capabilities.Set(capability.ReportStatus)
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
			return nil, false, false, fmt.Errorf("target does not support delete-refs")
		}
		_ = req.Capabilities.Set(capability.DeleteRefs)
	}

	_ = verbose // progress handling is server-side in HTTP mode
	return req, hasDelete, hasUpdates, nil
}

// sendReceivePack encodes and POSTs a receive-pack request, then decodes the report.
func sendReceivePack(
	ctx context.Context,
	conn *Conn,
	req *packp.UpdateRequests,
	packData io.Reader,
	verbose bool,
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
		dem.Progress = progressSink(verbose, "target: ")
		respReader = dem
	case req.Capabilities.Supports(capability.Sideband):
		dem := sideband.NewDemuxer(sideband.Sideband, reader)
		dem.Progress = progressSink(verbose, "target: ")
		respReader = dem
	}

	if req.Capabilities.Supports(capability.ReportStatus) {
		report := packp.NewReportStatus()
		if err := report.Decode(respReader); err != nil {
			return fmt.Errorf("decode report-status: %w", err)
		}
		if err := report.Error(); err != nil {
			return err
		}
	}
	return nil
}

// PushObjects pushes locally-materialized objects to the target.
func PushObjects(
	ctx context.Context,
	conn *Conn,
	adv *packp.AdvRefs,
	commands []PushCommand,
	store storer.Storer,
	hashes []plumbing.Hash,
	verbose bool,
) error {
	req, _, hasUpdates, err := buildUpdateRequest(adv, commands, verbose)
	if err != nil {
		return err
	}
	if !hasUpdates {
		return sendReceivePack(ctx, conn, req, nil, verbose)
	}

	useRefDeltas := !adv.Capabilities.Supports(capability.OFSDelta)
	pr, pw := io.Pipe()
	done := make(chan error, 1)

	go func() {
		enc := packfile.NewEncoder(pw, store, useRefDeltas)
		if _, err := enc.Encode(hashes, 10); err != nil {
			done <- pw.CloseWithError(fmt.Errorf("encode packfile: %w", err))
			return
		}
		done <- pw.Close()
	}()

	err = sendReceivePack(ctx, conn, req, pr, verbose)
	_ = pr.Close()
	encodeErr := <-done
	if err != nil {
		return err
	}
	return encodeErr
}

// PushPack pushes a pack stream (relay) to the target.
func PushPack(
	ctx context.Context,
	conn *Conn,
	adv *packp.AdvRefs,
	commands []PushCommand,
	pack io.ReadCloser,
	verbose bool,
) error {
	for _, cmd := range commands {
		if cmd.Delete {
			_ = pack.Close()
			return fmt.Errorf("pack push only supports create and update actions")
		}
	}

	req, _, _, err := buildUpdateRequest(adv, commands, verbose)
	if err != nil {
		_ = pack.Close()
		return err
	}

	err = sendReceivePack(ctx, conn, req, pack, verbose)
	closeErr := pack.Close()
	if err != nil {
		return err
	}
	return closeErr
}

// PushCommands sends ref update commands without a pack (for ref-only changes).
func PushCommands(
	ctx context.Context,
	conn *Conn,
	adv *packp.AdvRefs,
	commands []PushCommand,
	verbose bool,
) error {
	req, _, _, err := buildUpdateRequest(adv, commands, verbose)
	if err != nil {
		return err
	}
	return sendReceivePack(ctx, conn, req, nil, verbose)
}

func progressWriter(verbose bool) io.Writer {
	if !verbose {
		return nil
	}
	return os.Stderr
}

// progressSink returns a line-prefixing io.Writer suitable for
// sideband.Demuxer.Progress. When verbose is false it returns nil so the
// demuxer discards progress frames without allocating.
func progressSink(verbose bool, prefix string) io.Writer {
	if !verbose {
		return nil
	}
	return &prefixedLineWriter{w: os.Stderr, prefix: prefix, atLineStart: true}
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
				return consumed, err
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
			return consumed, err
		}
		b = b[len(chunk):]
	}
	return consumed, nil
}
