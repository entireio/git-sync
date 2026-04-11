package gitproto

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/go-git/go-git/v5/plumbing/transport"
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

// preparePush opens a receive-pack session and builds the base request with
// sideband negotiation. Shared by PushObjects, PushPack, and PushCommands.
func preparePush(
	conn *Conn,
	adv *packp.AdvRefs,
	commands []PushCommand,
	verbose bool,
) (transport.ReceivePackSession, *packp.ReferenceUpdateRequest, bool, bool, error) {
	session, err := conn.Transport.NewReceivePackSession(conn.Endpoint, conn.Auth)
	if err != nil {
		return nil, nil, false, false, fmt.Errorf("open target receive-pack session: %w", err)
	}

	req := packp.NewReferenceUpdateRequestFromCapabilities(adv.Capabilities)
	req.Progress = progressWriter(verbose)
	if sb := PreferredSideband(adv.Capabilities); sb != "" {
		_ = req.Capabilities.Set(sb)
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
			_ = session.Close()
			return nil, nil, false, false, fmt.Errorf("target does not support delete-refs")
		}
		_ = req.Capabilities.Set(capability.DeleteRefs)
	}

	return session, req, hasDelete, hasUpdates, nil
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
	session, req, _, hasUpdates, err := preparePush(conn, adv, commands, verbose)
	if err != nil {
		return err
	}
	defer session.Close()
	return executePush(ctx, session, store, req, hashes, hasUpdates, !adv.Capabilities.Supports(capability.OFSDelta))
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
	// Validate no deletes in pack push before opening session.
	for _, cmd := range commands {
		if cmd.Delete {
			return fmt.Errorf("pack push only supports create and update actions")
		}
	}

	session, req, _, _, err := preparePush(conn, adv, commands, verbose)
	if err != nil {
		return err
	}
	defer session.Close()

	req.Packfile = pack
	report, err := session.ReceivePack(ctx, req)
	if err != nil {
		_ = pack.Close()
		return err
	}
	if closeErr := pack.Close(); closeErr != nil {
		return closeErr
	}
	if report != nil {
		return report.Error()
	}
	return nil
}

// PushCommands sends ref update commands without a pack (for ref-only changes).
func PushCommands(
	ctx context.Context,
	conn *Conn,
	adv *packp.AdvRefs,
	commands []PushCommand,
	verbose bool,
) error {
	session, req, _, _, err := preparePush(conn, adv, commands, verbose)
	if err != nil {
		return err
	}
	defer session.Close()
	return executePush(ctx, session, nil, req, nil, false, false)
}

func executePush(
	ctx context.Context,
	session transport.ReceivePackSession,
	store storer.Storer,
	req *packp.ReferenceUpdateRequest,
	hashes []plumbing.Hash,
	sendPack bool,
	useRefDeltas bool,
) error {
	if !sendPack {
		report, err := session.ReceivePack(ctx, req)
		if err != nil {
			return err
		}
		if report != nil {
			return report.Error()
		}
		return nil
	}

	rd, wr := io.Pipe()
	req.Packfile = rd
	done := make(chan error, 1)

	go func() {
		enc := packfile.NewEncoder(wr, store, useRefDeltas)
		if _, err := enc.Encode(hashes, 10); err != nil {
			done <- wr.CloseWithError(err)
			return
		}
		done <- wr.Close()
	}()

	report, err := session.ReceivePack(ctx, req)
	if err != nil {
		_ = rd.Close()
		return err
	}
	if err := <-done; err != nil {
		return err
	}
	if report != nil {
		return report.Error()
	}
	return nil
}

func progressWriter(verbose bool) io.Writer {
	if !verbose {
		return nil
	}
	return os.Stderr
}
