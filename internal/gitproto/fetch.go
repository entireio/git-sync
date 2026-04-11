package gitproto

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/sideband"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/utils/ioutil"
)

// DesiredRef describes a single ref we want to fetch from source.
type DesiredRef struct {
	SourceRef  plumbing.ReferenceName
	TargetRef  plumbing.ReferenceName
	SourceHash plumbing.Hash
	IsTag      bool
}

// FetchToStore fetches objects from source into the given store, using the
// appropriate protocol version.
func (s *RefService) FetchToStore(
	ctx context.Context,
	store storer.Storer,
	conn *Conn,
	desired map[plumbing.ReferenceName]DesiredRef,
	targetRefs map[plumbing.ReferenceName]plumbing.Hash,
) error {
	switch s.Protocol {
	case "v2":
		return fetchToStoreV2(ctx, store, conn, s.V2Caps, desired, targetRefs)
	case "v1":
		return fetchToStoreV1(ctx, store, conn, s.V1Adv, desired, targetRefs)
	default:
		return fmt.Errorf("unsupported source protocol %q", s.Protocol)
	}
}

// FetchPack fetches a packfile from source and returns the pack stream as a reader.
// Caller must close the returned ReadCloser.
func (s *RefService) FetchPack(
	ctx context.Context,
	conn *Conn,
	desired map[plumbing.ReferenceName]DesiredRef,
	targetRefs map[plumbing.ReferenceName]plumbing.Hash,
) (io.ReadCloser, error) {
	switch s.Protocol {
	case "v2":
		return fetchPackV2(ctx, conn, s.V2Caps, desired, targetRefs)
	case "v1":
		return fetchPackV1(ctx, conn, s.V1Adv, desired, targetRefs)
	default:
		return nil, fmt.Errorf("unsupported source protocol %q", s.Protocol)
	}
}

// FetchCommitGraph fetches only the commit graph (tree:0 filter) for a ref.
// Requires v2 with filter support.
func (s *RefService) FetchCommitGraph(
	ctx context.Context,
	store storer.Storer,
	conn *Conn,
	ref DesiredRef,
) error {
	if s.Protocol != "v2" {
		return fmt.Errorf("commit graph fetch requires protocol v2")
	}
	if !s.V2Caps.FetchSupports("filter") {
		return fmt.Errorf("source does not advertise fetch filter support")
	}

	cmdArgs := []string{
		"ofs-delta",
		"no-progress",
		"filter tree:0",
		"want " + ref.SourceHash.String(),
		"done",
	}
	body, err := EncodeCommand("fetch", s.V2Caps.RequestCapabilities(), cmdArgs)
	if err != nil {
		return err
	}
	reader, err := PostRPCStream(ctx, conn, transport.UploadPackServiceName, body, true, "upload-pack fetch")
	if err != nil {
		return err
	}
	defer ioutil.CheckClose(reader, &err)
	return storeV2FetchPack(store, reader)
}

// Capabilities returns the sorted capability list for display.
func (s *RefService) Capabilities() []string {
	switch s.Protocol {
	case "v2":
		return s.V2Caps.SortedKeys()
	case "v1":
		return AdvRefsCaps(s.V1Adv)
	default:
		return nil
	}
}

// --- V2 fetch implementation ---

func fetchToStoreV2(
	ctx context.Context,
	store storer.Storer,
	conn *Conn,
	caps *V2Capabilities,
	desired map[plumbing.ReferenceName]DesiredRef,
	targetRefs map[plumbing.ReferenceName]plumbing.Hash,
) error {
	wants := collectWants(desired)
	haves := SortedUniqueHashes(refValues(targetRefs))
	if len(wants) == 0 {
		return git.NoErrAlreadyUpToDate
	}

	cmdArgs := make([]string, 0, len(wants)+len(haves)+4)
	cmdArgs = append(cmdArgs, "ofs-delta", "no-progress")
	for _, h := range wants {
		cmdArgs = append(cmdArgs, "want "+h.String())
	}
	for _, h := range haves {
		cmdArgs = append(cmdArgs, "have "+h.String())
	}
	cmdArgs = append(cmdArgs, "done")

	body, err := EncodeCommand("fetch", caps.RequestCapabilities(), cmdArgs)
	if err != nil {
		return err
	}
	reader, err := PostRPCStream(ctx, conn, transport.UploadPackServiceName, body, true, "upload-pack fetch")
	if err != nil {
		return err
	}
	defer ioutil.CheckClose(reader, &err)
	return storeV2FetchPack(store, reader)
}

func fetchPackV2(
	ctx context.Context,
	conn *Conn,
	caps *V2Capabilities,
	desired map[plumbing.ReferenceName]DesiredRef,
	targetRefs map[plumbing.ReferenceName]plumbing.Hash,
) (io.ReadCloser, error) {
	wants := collectWants(desired)
	haves := SortedUniqueHashes(refValues(targetRefs))
	if len(wants) == 0 {
		return nil, git.NoErrAlreadyUpToDate
	}

	cmdArgs := make([]string, 0, len(wants)+len(haves)+4)
	cmdArgs = append(cmdArgs, "ofs-delta", "no-progress")
	// Only request include-tag if the server supports it (issue #6).
	if hasTag(desired) && caps.FetchSupports("include-tag") {
		cmdArgs = append(cmdArgs, "include-tag")
	}
	for _, h := range wants {
		cmdArgs = append(cmdArgs, "want "+h.String())
	}
	for _, h := range haves {
		cmdArgs = append(cmdArgs, "have "+h.String())
	}
	cmdArgs = append(cmdArgs, "done")

	body, err := EncodeCommand("fetch", caps.RequestCapabilities(), cmdArgs)
	if err != nil {
		return nil, err
	}
	reader, err := PostRPCStream(ctx, conn, transport.UploadPackServiceName, body, true, "upload-pack fetch")
	if err != nil {
		return nil, err
	}
	packStream, err := openV2PackStream(reader)
	if err != nil {
		_ = reader.Close()
		return nil, err
	}
	return packStream, nil
}

func storeV2FetchPack(store storer.Storer, r io.Reader) error {
	reader := NewPacketReader(r)
	for {
		kind, payload, err := reader.ReadPacket()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("decode protocol v2 fetch response: %w", err)
		}
		switch kind {
		case PacketFlush:
			return nil
		case PacketDelim, PacketResponseEnd:
			continue
		case PacketData:
			line := string(payload)
			switch line {
			case "packfile\n":
				demux := sideband.NewDemuxer(sideband.Sideband64k, reader.BufReader())
				return packfile.UpdateObjectStorage(store, demux)
			case "acknowledgments\n", "shallow-info\n":
				if err := SkipSection(reader); err != nil {
					return err
				}
			default:
				return fmt.Errorf("unexpected protocol v2 fetch section %q", strings.TrimSpace(line))
			}
		}
	}
}

func openV2PackStream(body io.ReadCloser) (io.ReadCloser, error) {
	reader := NewPacketReader(body)
	for {
		kind, payload, err := reader.ReadPacket()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil, io.ErrUnexpectedEOF
			}
			return nil, fmt.Errorf("decode protocol v2 fetch response: %w", err)
		}
		switch kind {
		case PacketFlush:
			return nil, io.ErrUnexpectedEOF
		case PacketDelim, PacketResponseEnd:
			continue
		case PacketData:
			line := string(payload)
			switch line {
			case "packfile\n":
				return &wrappedRC{
					Reader: sideband.NewDemuxer(sideband.Sideband64k, reader.BufReader()),
					Closer: body,
				}, nil
			case "acknowledgments\n", "shallow-info\n":
				if err := SkipSection(reader); err != nil {
					return nil, err
				}
			default:
				return nil, fmt.Errorf("unexpected protocol v2 fetch section %q", strings.TrimSpace(line))
			}
		}
	}
}

// --- V1 fetch implementation ---

func fetchToStoreV1(
	ctx context.Context,
	store storer.Storer,
	conn *Conn,
	adv *packp.AdvRefs,
	desired map[plumbing.ReferenceName]DesiredRef,
	targetRefs map[plumbing.ReferenceName]plumbing.Hash,
) error {
	session, err := conn.Transport.NewUploadPackSession(conn.Endpoint, conn.Auth)
	if err != nil {
		return fmt.Errorf("open source upload-pack session: %w", err)
	}
	defer session.Close()

	req := packp.NewUploadPackRequestFromCapabilities(adv.Capabilities)
	for _, ref := range desired {
		req.Wants = append(req.Wants, ref.SourceHash)
	}
	req.Wants = SortedUniqueHashes(req.Wants)
	req.Haves = SortedUniqueHashes(refValues(targetRefs))
	if len(req.Wants) == 0 {
		return git.NoErrAlreadyUpToDate
	}
	if adv.Capabilities.Supports(capability.NoProgress) {
		_ = req.Capabilities.Set(capability.NoProgress)
	}
	if hasTag(desired) && adv.Capabilities.Supports(capability.IncludeTag) {
		_ = req.Capabilities.Set(capability.IncludeTag)
	}

	reader, err := session.UploadPack(ctx, req)
	if err != nil {
		if errors.Is(err, transport.ErrEmptyUploadPackRequest) {
			return git.NoErrAlreadyUpToDate
		}
		return fmt.Errorf("source upload-pack: %w", err)
	}
	defer ioutil.CheckClose(reader, &err)

	sbReader := buildSidebandReader(req.Capabilities, reader, nil)
	return packfile.UpdateObjectStorage(store, sbReader)
}

func fetchPackV1(
	ctx context.Context,
	conn *Conn,
	adv *packp.AdvRefs,
	desired map[plumbing.ReferenceName]DesiredRef,
	targetRefs map[plumbing.ReferenceName]plumbing.Hash,
) (io.ReadCloser, error) {
	session, err := conn.Transport.NewUploadPackSession(conn.Endpoint, conn.Auth)
	if err != nil {
		return nil, fmt.Errorf("open source upload-pack session: %w", err)
	}

	req := packp.NewUploadPackRequestFromCapabilities(adv.Capabilities)
	for _, ref := range desired {
		req.Wants = append(req.Wants, ref.SourceHash)
	}
	req.Wants = SortedUniqueHashes(req.Wants)
	req.Haves = SortedUniqueHashes(refValues(targetRefs))
	if len(req.Wants) == 0 {
		_ = session.Close()
		return nil, git.NoErrAlreadyUpToDate
	}
	if adv.Capabilities.Supports(capability.NoProgress) {
		_ = req.Capabilities.Set(capability.NoProgress)
	}
	if hasTag(desired) && adv.Capabilities.Supports(capability.IncludeTag) {
		_ = req.Capabilities.Set(capability.IncludeTag)
	}

	reader, err := session.UploadPack(ctx, req)
	if err != nil {
		_ = session.Close()
		if errors.Is(err, transport.ErrEmptyUploadPackRequest) {
			return nil, git.NoErrAlreadyUpToDate
		}
		return nil, fmt.Errorf("source upload-pack: %w", err)
	}
	return &sessionRC{
		Reader: buildSidebandReader(req.Capabilities, reader, nil),
		closeFn: func() error {
			_ = reader.Close()
			return session.Close()
		},
	}, nil
}

// buildSidebandReader wraps a reader with sideband demuxing if the negotiated
// capabilities include sideband support. Delegates to PreferredSideband (issue #4).
func buildSidebandReader(caps *capability.List, reader io.Reader, progress sideband.Progress) io.Reader {
	sb := PreferredSideband(caps)
	if sb == "" {
		return reader
	}
	var t sideband.Type
	if sb == capability.Sideband64k {
		t = sideband.Sideband64k
	} else {
		t = sideband.Sideband
	}
	d := sideband.NewDemuxer(t, reader)
	d.Progress = progress
	return d
}

// --- helpers ---

func collectWants(desired map[plumbing.ReferenceName]DesiredRef) []plumbing.Hash {
	hashes := make([]plumbing.Hash, 0, len(desired))
	for _, ref := range desired {
		hashes = append(hashes, ref.SourceHash)
	}
	return SortedUniqueHashes(hashes)
}

func hasTag(desired map[plumbing.ReferenceName]DesiredRef) bool {
	for _, ref := range desired {
		if ref.IsTag {
			return true
		}
	}
	return false
}

func refValues(m map[plumbing.ReferenceName]plumbing.Hash) []plumbing.Hash {
	out := make([]plumbing.Hash, 0, len(m))
	for _, h := range m {
		if !h.IsZero() {
			out = append(out, h)
		}
	}
	return out
}

// SortedUniqueHashes deduplicates and sorts a hash slice.
func SortedUniqueHashes(input []plumbing.Hash) []plumbing.Hash {
	seen := make(map[plumbing.Hash]struct{}, len(input))
	out := make([]plumbing.Hash, 0, len(input))
	for _, h := range input {
		if _, ok := seen[h]; ok {
			continue
		}
		seen[h] = struct{}{}
		out = append(out, h)
	}
	plumbing.HashesSort(out)
	return out
}

type wrappedRC struct {
	io.Reader
	io.Closer
}

type sessionRC struct {
	io.Reader
	closeFn func() error
}

func (r *sessionRC) Close() error {
	if r.closeFn == nil {
		return nil
	}
	return r.closeFn()
}
