package gitproto

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/go-git/go-git/v6/plumbing"
	formatcfg "github.com/go-git/go-git/v6/plumbing/format/config"
	"github.com/go-git/go-git/v6/plumbing/protocol/capability"
	"github.com/go-git/go-git/v6/plumbing/protocol/packp"
	"github.com/go-git/go-git/v6/plumbing/transport"
	"github.com/go-git/go-git/v6/storage/memory"

	"entire.io/entire/git-sync/internal/sanitize"
)

// HasCommits asks the server which of commits it holds. It negotiates the
// way a fetch does: it wants one of tips, which must come from this
// service's advertisement, and offers commits as haves. upload-pack
// acknowledges each have it holds and sends no pack, because the request
// never says "done".
//
// Wanting the commits instead would fail over stateful protocol v0 and v1,
// such as SSH, where upload-pack refuses a want no ref points at. It also
// could not tell a refusal from a missing commit.
//
// upload-pack v2 leaves out a have when it read that commit's child earlier
// in the same request. So HasCommits offers the commits left unacknowledged
// again, together, until a request acknowledges none of them. The first
// held commit of each request is always acknowledged, so the commits left
// then are missing.
//
// A v2 server that does not advertise wait-for-done would send a pack once
// the haves cover the want. There HasCommits falls back to wanting the
// commits while claiming to have them, which v2 allows for any commit a ref
// reaches. That fallback cannot tell which commit is missing, so it returns
// an error instead.
//
// A target without refs gets no request. v2 would accept haves without a
// want, but a commit no ref reaches is not one a sync can build on.
func (s *RefService) HasCommits(ctx context.Context, conn Conn, tips, commits []plumbing.Hash) (map[plumbing.Hash]struct{}, error) {
	commits = SortedUniqueHashes(commits)
	tips = slices.DeleteFunc(SortedUniqueHashes(tips), plumbing.Hash.IsZero)
	if len(commits) == 0 || len(tips) == 0 {
		// With no tips there is nothing to want. A repository without refs
		// holds no commits a client can name.
		return map[plumbing.Hash]struct{}{}, nil
	}
	held := make(map[plumbing.Hash]struct{}, len(commits))
	for len(commits) > 0 {
		acked, err := s.offerHaves(ctx, conn, tips[0], commits)
		if err != nil {
			return nil, err
		}
		if len(acked) == 0 {
			break
		}
		maps.Copy(held, acked)
		commits = slices.DeleteFunc(commits, func(h plumbing.Hash) bool {
			_, ok := acked[h]
			return ok
		})
	}
	return held, nil
}

// remoteError reports the text of an ERR packet. A server controls that
// text, so it is kept to one line without control characters.
func remoteError(msg string) error {
	return errors.New("remote: " + sanitize.Line(msg))
}

func (s *RefService) offerHaves(ctx context.Context, conn Conn, tip plumbing.Hash, commits []plumbing.Hash) (map[plumbing.Hash]struct{}, error) {
	if s.Protocol == "v2" {
		return hasCommitsV2(ctx, conn, s.V2Caps, tip, commits)
	}
	return hasCommitsV1(ctx, conn, s.V1Adv, tip, commits)
}

func hasCommitsV1(ctx context.Context, conn Conn, adv *packp.AdvRefs, tip plumbing.Hash, commits []plumbing.Hash) (map[plumbing.Hash]struct{}, error) {
	if !adv.Capabilities.Supports(capability.MultiACKDetailed) {
		return nil, errors.New("upload-pack does not support multi_ack_detailed")
	}
	req := &packp.UploadRequest{Wants: []plumbing.Hash{tip}}
	req.Capabilities.Set(capability.MultiACKDetailed)
	if format := AdvRefsObjectFormat(adv); format != formatcfg.UnsetObjectFormat {
		req.Capabilities.Set(capability.ObjectFormat, string(format))
	}
	var body bytes.Buffer
	if err := req.Encode(&body); err != nil {
		return nil, fmt.Errorf("encode upload-request: %w", err)
	}
	haves := &packp.UploadHaves{Haves: append([]plumbing.Hash(nil), commits...)}
	if err := haves.Encode(&body); err != nil {
		return nil, fmt.Errorf("encode upload-haves: %w", err)
	}
	reader, err := PostRPCStream(ctx, conn, transport.UploadPackService, body.Bytes(), false, "upload-pack negotiate")
	if err != nil {
		return nil, fmt.Errorf("upload-pack: %w", err)
	}
	common, err := parseCommonAcksV1(NewPacketReader(reader))
	// A stateful upload-pack waits for more haves and dies when the input
	// ends, so closing reports an error even after a complete answer.
	_ = reader.Close()
	return common, err
}

// parseCommonAcksV1 reads multi_ack_detailed acknowledgments up to the NAK
// that ends the round. Only "ACK <oid> common" means the server holds oid.
// "ACK <oid> ready" also names haves it lacks.
func parseCommonAcksV1(r *PacketReader) (map[plumbing.Hash]struct{}, error) {
	common := make(map[plumbing.Hash]struct{})
	for {
		kind, payload, err := r.ReadPacket()
		if err != nil {
			return nil, err
		}
		if kind != PacketData {
			return nil, fmt.Errorf("unexpected packet type %v in acknowledgments", kind)
		}
		line := strings.TrimSuffix(string(payload), "\n")
		switch {
		case line == "NAK":
			return common, nil
		case strings.HasPrefix(line, "ERR "):
			return nil, remoteError(line[4:])
		case strings.HasPrefix(line, "ACK "):
			fields := strings.Fields(line)
			if len(fields) == 3 && fields[2] == "common" {
				h, ok := plumbing.FromHex(fields[1])
				if !ok {
					return nil, fmt.Errorf("malformed acknowledgment %q", line)
				}
				common[h] = struct{}{}
			}
		default:
			return nil, fmt.Errorf("unexpected acknowledgment %q", line)
		}
	}
}

func hasCommitsV2(ctx context.Context, conn Conn, caps *V2Capabilities, tip plumbing.Hash, commits []plumbing.Hash) (map[plumbing.Hash]struct{}, error) {
	if !caps.FetchSupports("wait-for-done") {
		return wantCommitsV2(ctx, conn, caps, commits)
	}
	args := make([]string, 0, len(commits)+2)
	args = append(args, "wait-for-done", "want "+tip.String())
	for _, h := range commits {
		args = append(args, "have "+h.String())
	}
	body, err := EncodeCommand("fetch", caps.RequestCapabilities(), args)
	if err != nil {
		return nil, err
	}
	reader, err := PostRPCStream(ctx, conn, transport.UploadPackService, body, true, "upload-pack negotiate")
	if err != nil {
		return nil, fmt.Errorf("upload-pack: %w", err)
	}
	common, err := parseCommonAcksV2(NewPacketReader(reader))
	_ = reader.Close()
	return common, err
}

// parseCommonAcksV2 reads the acknowledgments section of a v2 fetch
// response that sends no pack. Each "ACK <oid>" names a have the server
// holds.
func parseCommonAcksV2(r *PacketReader) (map[plumbing.Hash]struct{}, error) {
	common := make(map[plumbing.Hash]struct{})
	inSection := false
	for {
		kind, payload, err := r.ReadPacket()
		if err != nil {
			return nil, err
		}
		switch kind {
		case PacketFlush, PacketResponseEnd:
			if !inSection {
				return nil, errors.New("fetch response has no acknowledgments")
			}
			return common, nil
		case PacketDelim:
			return nil, errors.New("server is ready to send a pack it was not asked for")
		case PacketData:
		}
		line := strings.TrimSuffix(string(payload), "\n")
		switch {
		case strings.HasPrefix(line, "ERR "):
			return nil, remoteError(line[4:])
		case !inSection && line == "acknowledgments":
			inSection = true
		case inSection && line == "NAK":
		case inSection && line == "ready":
			return nil, errors.New("server is ready to send a pack it was not asked for")
		case inSection && strings.HasPrefix(line, "ACK "):
			h, ok := plumbing.FromHex(strings.TrimPrefix(line, "ACK "))
			if !ok {
				return nil, fmt.Errorf("malformed acknowledgment %q", line)
			}
			common[h] = struct{}{}
		default:
			return nil, fmt.Errorf("unexpected fetch response line %q", line)
		}
	}
}

// wantCommitsV2 wants commits while claiming to have them, so a server that
// holds them all sends an empty pack.
func wantCommitsV2(ctx context.Context, conn Conn, caps *V2Capabilities, commits []plumbing.Hash) (map[plumbing.Hash]struct{}, error) {
	desired := make(map[plumbing.ReferenceName]DesiredRef, len(commits))
	haves := make(map[plumbing.ReferenceName]plumbing.Hash, len(commits))
	for _, h := range commits {
		name := plumbing.ReferenceName("refs/git-sync/check/" + h.String())
		desired[name] = DesiredRef{SourceRef: name, TargetRef: name, SourceHash: h}
		haves[name] = h
	}
	store := memory.NewStorage(memory.WithObjectFormat(caps.ObjectFormat()))
	if err := fetchToStoreV2(ctx, store, conn, caps, desired, haves, false); err != nil {
		return nil, fmt.Errorf("upload-pack refused %d commit(s), or does not hold them: %w", len(commits), err)
	}
	common := make(map[plumbing.Hash]struct{}, len(commits))
	for _, h := range commits {
		common[h] = struct{}{}
	}
	return common, nil
}
