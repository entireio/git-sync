package gitproto

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/format/packfile"
	"github.com/go-git/go-git/v6/plumbing/protocol/packp"
	"github.com/go-git/go-git/v6/plumbing/protocol/packp/capability"
	"github.com/go-git/go-git/v6/plumbing/protocol/packp/sideband"
	"github.com/go-git/go-git/v6/plumbing/storer"
	"github.com/go-git/go-git/v6/plumbing/transport"
	"github.com/go-git/go-git/v6/utils/ioutil"
)

// DesiredRef describes a single ref we want to fetch from source.
type DesiredRef struct {
	SourceRef  plumbing.ReferenceName
	TargetRef  plumbing.ReferenceName
	SourceHash plumbing.Hash
	IsTag      bool
}

// FetchFeatures summarizes negotiated source fetch features used by strategies.
type FetchFeatures struct {
	Filter     bool
	IncludeTag bool
}

func (s *RefService) FetchFeatures() FetchFeatures {
	if s == nil || s.Protocol != "v2" || s.V2Caps == nil {
		return FetchFeatures{}
	}
	return FetchFeatures{
		Filter:     s.V2Caps.FetchSupports("filter"),
		IncludeTag: s.V2Caps.FetchSupports("include-tag"),
	}
}

// SupportsBootstrapBatch centralizes the source-side capability check for the
// batched bootstrap strategy.
func (s *RefService) SupportsBootstrapBatch() bool {
	return s != nil && s.Protocol == "v2" && s.FetchFeatures().Filter
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
		return fetchToStoreV2(ctx, store, conn, s.V2Caps, desired, targetRefs, s.Verbose)
	case "v1":
		return fetchToStoreV1(ctx, store, conn, s.V1Adv, desired, targetRefs, s.Verbose)
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
		return fetchPackV2(ctx, conn, s.V2Caps, desired, targetRefs, s.Verbose)
	case "v1":
		return fetchPackV1(ctx, conn, s.V1Adv, desired, targetRefs, s.Verbose)
	default:
		return nil, fmt.Errorf("unsupported source protocol %q", s.Protocol)
	}
}

// FetchCommitGraph fetches only the commit graph (tree:0 filter) for a ref.
// Requires v2 with filter support. Optional haves let the source skip commits
// already reachable from those hashes, which is valuable when planning later
// branches that share history with an already-planned trunk.
func (s *RefService) FetchCommitGraph(
	ctx context.Context,
	store storer.Storer,
	conn *Conn,
	ref DesiredRef,
	haves []plumbing.Hash,
) error {
	if s.Protocol != "v2" {
		return errors.New("commit graph fetch requires protocol v2")
	}
	if !s.V2Caps.FetchSupports("filter") {
		return errors.New("source does not advertise fetch filter support")
	}

	sortedHaves := SortedUniqueHashes(haves)
	cmdArgs := make([]string, 0, 4+len(sortedHaves))
	cmdArgs = append(cmdArgs,
		"ofs-delta",
		"no-progress",
		"filter tree:0",
		"want "+ref.SourceHash.String(),
	)
	for _, h := range sortedHaves {
		cmdArgs = append(cmdArgs, "have "+h.String())
	}
	cmdArgs = append(cmdArgs, "done")

	body, err := EncodeCommand("fetch", s.V2Caps.RequestCapabilities(), cmdArgs)
	if err != nil {
		return err
	}
	reader, err := PostRPCStream(ctx, conn, transport.UploadPackService, body, true, "upload-pack fetch")
	if err != nil {
		return err
	}
	defer ioutil.CheckClose(reader, &err)
	// Commit-graph fetches are short and not user-facing; skip progress.
	return storeV2FetchPack(store, reader, false)
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
	verbose bool,
) error {
	wants := collectWants(desired)
	haves := SortedUniqueHashes(refValues(targetRefs))
	if len(wants) == 0 {
		return git.NoErrAlreadyUpToDate
	}

	cmdArgs := make([]string, 0, len(wants)+len(haves)+4)
	// NOTE: no "thin-pack" argument. The relayed pack must stay
	// self-contained so callers (e.g. replicate) can forward it to
	// receive-pack servers that may advertise "no-thin". See
	// planner.SupportsReplicateRelay for the matching invariant.
	cmdArgs = append(cmdArgs, "ofs-delta")
	if !verbose {
		cmdArgs = append(cmdArgs, "no-progress")
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
		return err
	}
	reader, err := PostRPCStream(ctx, conn, transport.UploadPackService, body, true, "upload-pack fetch")
	if err != nil {
		return err
	}
	defer ioutil.CheckClose(reader, &err)
	return storeV2FetchPack(store, reader, verbose)
}

func fetchPackV2(
	ctx context.Context,
	conn *Conn,
	caps *V2Capabilities,
	desired map[plumbing.ReferenceName]DesiredRef,
	targetRefs map[plumbing.ReferenceName]plumbing.Hash,
	verbose bool,
) (io.ReadCloser, error) {
	wants := collectWants(desired)
	haves := SortedUniqueHashes(refValues(targetRefs))
	if len(wants) == 0 {
		return nil, git.NoErrAlreadyUpToDate
	}

	cmdArgs := make([]string, 0, len(wants)+len(haves)+4)
	// NOTE: no "thin-pack" argument. The relayed pack must stay
	// self-contained so callers (e.g. replicate) can forward it to
	// receive-pack servers that may advertise "no-thin". See
	// planner.SupportsReplicateRelay for the matching invariant.
	cmdArgs = append(cmdArgs, "ofs-delta")
	if !verbose {
		cmdArgs = append(cmdArgs, "no-progress")
	}
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
	reader, err := PostRPCStream(ctx, conn, transport.UploadPackService, body, true, "upload-pack fetch")
	if err != nil {
		return nil, err
	}
	packStream, err := openV2PackStream(reader, verbose)
	if err != nil {
		_ = reader.Close()
		return nil, err
	}
	return packStream, nil
}

func storeV2FetchPack(store storer.Storer, r io.Reader, verbose bool) error {
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
				demux.Progress = progressSink(verbose, "source: ")
				if err := packfile.UpdateObjectStorage(store, demux); err != nil {
					return fmt.Errorf("update object storage: %w", err)
				}
				return nil
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

func openV2PackStream(body io.ReadCloser, verbose bool) (io.ReadCloser, error) {
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
				demux := sideband.NewDemuxer(sideband.Sideband64k, reader.BufReader())
				demux.Progress = progressSink(verbose, "source: ")
				return &wrappedRC{
					Reader: demux,
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

// buildV1UploadPackBody encodes a v1 upload-pack request body for stateless-rpc HTTP.
func buildV1UploadPackBody(
	adv *packp.AdvRefs,
	desired map[plumbing.ReferenceName]DesiredRef,
	targetRefs map[plumbing.ReferenceName]plumbing.Hash,
	includeTags bool,
	verbose bool,
) ([]byte, *capability.List, error) {
	wants := collectWants(desired)
	haves := SortedUniqueHashes(refValues(targetRefs))
	if len(wants) == 0 {
		return nil, nil, git.NoErrAlreadyUpToDate
	}

	req := packp.NewUploadRequest()
	req.Wants = wants
	if !verbose && adv.Capabilities.Supports(capability.NoProgress) {
		if err := req.Capabilities.Set(capability.NoProgress); err != nil {
			return nil, nil, fmt.Errorf("set capability: %w", err)
		}
	}
	if includeTags && adv.Capabilities.Supports(capability.IncludeTag) {
		if err := req.Capabilities.Set(capability.IncludeTag); err != nil {
			return nil, nil, fmt.Errorf("set capability: %w", err)
		}
	}
	// Prefer sideband64k over sideband (issue #4).
	if sb := PreferredSideband(adv.Capabilities); sb != "" {
		if err := req.Capabilities.Set(sb); err != nil {
			return nil, nil, fmt.Errorf("set capability: %w", err)
		}
	}
	if adv.Capabilities.Supports(capability.OFSDelta) {
		if err := req.Capabilities.Set(capability.OFSDelta); err != nil {
			return nil, nil, fmt.Errorf("set capability: %w", err)
		}
	}
	// NOTE: we intentionally do not request capability.ThinPack. The relayed
	// pack must stay self-contained because callers (e.g. replicate) forward
	// it to receive-pack servers that may advertise "no-thin".
	// planner.SupportsReplicateRelay depends on this invariant — if you add
	// thin-pack support here, update that check to gate on target NoThin.

	var buf bytes.Buffer
	if err := req.Encode(&buf); err != nil {
		return nil, nil, fmt.Errorf("encode upload-request: %w", err)
	}
	uphav := &packp.UploadHaves{Haves: haves, Done: true}
	if err := uphav.Encode(&buf); err != nil {
		return nil, nil, fmt.Errorf("encode upload-haves: %w", err)
	}
	return buf.Bytes(), req.Capabilities, nil
}

func fetchToStoreV1(
	ctx context.Context,
	store storer.Storer,
	conn *Conn,
	adv *packp.AdvRefs,
	desired map[plumbing.ReferenceName]DesiredRef,
	targetRefs map[plumbing.ReferenceName]plumbing.Hash,
	verbose bool,
) error {
	body, caps, err := buildV1UploadPackBody(adv, desired, targetRefs, hasTag(desired), verbose)
	if err != nil {
		return err
	}
	reader, err := PostRPCStream(ctx, conn, transport.UploadPackService, body, false, "upload-pack fetch")
	if err != nil {
		return fmt.Errorf("source upload-pack: %w", err)
	}
	defer ioutil.CheckClose(reader, &err)

	// Decode server response (ACK/NAK) then read pack with sideband demux.
	buffered := bufio.NewReader(reader)
	var srvResp packp.ServerResponse
	if decErr := srvResp.Decode(buffered); decErr != nil {
		return fmt.Errorf("decode server response: %w", decErr)
	}
	if drainErr := drainTrailingNAKs(buffered); drainErr != nil {
		return fmt.Errorf("drain server response: %w", drainErr)
	}
	sbReader := buildSidebandReader(caps, buffered, progressSink(verbose, "source: "))
	if err := packfile.UpdateObjectStorage(store, sbReader); err != nil {
		return fmt.Errorf("update object storage: %w", err)
	}
	return nil
}

func fetchPackV1(
	ctx context.Context,
	conn *Conn,
	adv *packp.AdvRefs,
	desired map[plumbing.ReferenceName]DesiredRef,
	targetRefs map[plumbing.ReferenceName]plumbing.Hash,
	verbose bool,
) (io.ReadCloser, error) {
	body, caps, err := buildV1UploadPackBody(adv, desired, targetRefs, hasTag(desired), verbose)
	if err != nil {
		return nil, err
	}
	reader, err := PostRPCStream(ctx, conn, transport.UploadPackService, body, false, "upload-pack fetch")
	if err != nil {
		return nil, fmt.Errorf("source upload-pack: %w", err)
	}

	buffered := bufio.NewReader(reader)
	var srvResp packp.ServerResponse
	if decErr := srvResp.Decode(buffered); decErr != nil {
		_ = reader.Close()
		return nil, fmt.Errorf("decode server response: %w", decErr)
	}
	if drainErr := drainTrailingNAKs(buffered); drainErr != nil {
		_ = reader.Close()
		return nil, fmt.Errorf("drain server response: %w", drainErr)
	}
	return &wrappedRC{
		Reader: buildSidebandReader(caps, buffered, progressSink(verbose, "source: ")),
		Closer: reader,
	}, nil
}

// drainTrailingNAKs consumes any extra "NAK\n" pktlines left in the stream
// after ServerResponse.Decode. go-git's upload-pack server emits a second NAK
// when haves were sent but none were reachable from the wants (see
// plumbing/transport/upload_pack.go), while go-git's ServerResponse.Decode
// stops after the first NAK. The remainder would otherwise be misread by the
// sideband demuxer as a frame with channel byte 'N' ("unknown channel NAK").
//
// A stream that runs out before we can peek 8 bytes carries no trailing NAK
// to drain, so we silently stop. The downstream consumer observes the same
// underlying read error on its first read.
func drainTrailingNAKs(r *bufio.Reader) error {
	for {
		header, err := r.Peek(8)
		if len(header) < 8 || !bytes.Equal(header, []byte("0008NAK\n")) {
			_ = err
			return nil
		}
		if _, err := r.Discard(8); err != nil {
			return fmt.Errorf("discard trailing NAK: %w", err)
		}
	}
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
