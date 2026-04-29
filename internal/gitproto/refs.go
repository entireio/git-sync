package gitproto

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/format/pktline"
	"github.com/go-git/go-git/v6/plumbing/protocol/packp"
	"github.com/go-git/go-git/v6/plumbing/protocol/packp/capability"
	"github.com/go-git/go-git/v6/plumbing/transport"
)

// RefService encapsulates the result of source ref discovery and the negotiated
// protocol, providing methods for subsequent fetch and pack operations.
type RefService struct {
	Protocol string // "v1" or "v2"
	V1Adv    *packp.AdvRefs
	V2Caps   *V2Capabilities
	// HeadTarget is the branch that HEAD points to on the source, when
	// advertised as a symref. Empty for detached HEAD or when the source
	// does not advertise symref information.
	HeadTarget plumbing.ReferenceName
	// Verbose, when true, streams source-side sideband progress ("Counting
	// objects", "Compressing objects", ...) to stderr and asks the source
	// upload-pack to emit progress by not sending the no-progress option.
	Verbose bool
}

// ListSourceRefs discovers refs from the source using the configured protocol mode.
// Returns the list of refs and a RefService for subsequent operations.
func ListSourceRefs(ctx context.Context, conn *Conn, protocolMode string, refPrefixes []string) ([]*plumbing.Reference, *RefService, error) {
	switch protocolMode {
	case "v1":
		adv, refs, err := listSourceRefsV1(ctx, conn)
		if err != nil {
			return nil, nil, err
		}
		return refs, &RefService{Protocol: "v1", V1Adv: adv, HeadTarget: headTargetFromAdv(adv)}, nil

	case "auto", "v2":
		data, err := RequestInfoRefs(ctx, conn, transport.UploadPackService, "version=2")
		if err != nil {
			return nil, nil, err
		}
		if caps, err := DecodeV2Capabilities(bytes.NewReader(data)); err == nil {
			if !caps.Supports("ls-refs") || !caps.Supports("fetch") {
				return nil, nil, errors.New("source does not advertise required protocol v2 commands")
			}
			refs, headTarget, err := listSourceRefsV2(ctx, conn, caps, refPrefixes)
			if err != nil {
				return nil, nil, err
			}
			return refs, &RefService{Protocol: "v2", V2Caps: caps, HeadTarget: headTarget}, nil
		}
		if protocolMode == "v2" {
			return nil, nil, errors.New("source did not negotiate protocol v2")
		}
		// Fall back to v1
		adv, err := decodeV1AdvRefs(data)
		if err != nil {
			return nil, nil, err
		}
		refs, err := AdvRefsToSlice(adv)
		if err != nil {
			return nil, nil, err
		}
		return refs, &RefService{Protocol: "v1", V1Adv: adv, HeadTarget: headTargetFromAdv(adv)}, nil

	default:
		return nil, nil, fmt.Errorf("unsupported protocol mode %q", protocolMode)
	}
}

// AdvertisedRefsV1 fetches and decodes v1 advertised refs for the given service.
func AdvertisedRefsV1(ctx context.Context, conn *Conn, service string) (*packp.AdvRefs, error) {
	data, err := RequestInfoRefs(ctx, conn, service, "")
	if err != nil {
		return nil, err
	}
	return decodeV1AdvRefs(data)
}

// AdvRefsToSlice converts an AdvRefs to a slice of references.
func AdvRefsToSlice(ar *packp.AdvRefs) ([]*plumbing.Reference, error) {
	refs, err := ar.AllReferences()
	if err != nil {
		return nil, fmt.Errorf("all references: %w", err)
	}
	iter, err := refs.IterReferences()
	if err != nil {
		return nil, fmt.Errorf("iter references: %w", err)
	}
	defer iter.Close()

	var out []*plumbing.Reference
	err = iter.ForEach(func(ref *plumbing.Reference) error {
		out = append(out, ref)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("iterate references: %w", err)
	}
	return out, nil
}

// AdvRefsCaps returns the sorted capability list from an AdvRefs.
func AdvRefsCaps(adv *packp.AdvRefs) []string {
	if adv == nil || adv.Capabilities == nil {
		return nil
	}
	all := adv.Capabilities.All()
	items := make([]string, 0, len(all))
	for _, cap := range all {
		values := adv.Capabilities.Get(cap)
		if len(values) == 0 {
			items = append(items, string(cap))
			continue
		}
		for _, value := range values {
			items = append(items, string(cap)+"="+value)
		}
	}
	return items
}

func listSourceRefsV1(ctx context.Context, conn *Conn) (*packp.AdvRefs, []*plumbing.Reference, error) {
	adv, err := AdvertisedRefsV1(ctx, conn, transport.UploadPackService)
	if err != nil {
		return nil, nil, err
	}
	refs, err := AdvRefsToSlice(adv)
	if err != nil {
		return nil, nil, err
	}
	return adv, refs, nil
}

func listSourceRefsV2(ctx context.Context, conn *Conn, caps *V2Capabilities, prefixes []string) ([]*plumbing.Reference, plumbing.ReferenceName, error) {
	// Always include "HEAD" so the server returns the symref-target attribute
	// for HEAD. Without this, callers that pass only "refs/heads/" or
	// "refs/tags/" prefixes filter HEAD out of the response and lose the
	// default-branch hint that bootstrap planning uses as a trunk cutoff.
	args := []string{"peel", "symrefs", "ref-prefix HEAD"}
	for _, prefix := range prefixes {
		args = append(args, "ref-prefix "+prefix)
	}
	body, err := EncodeCommand("ls-refs", caps.RequestCapabilities(), args)
	if err != nil {
		return nil, "", err
	}
	data, err := PostRPC(ctx, conn, transport.UploadPackService, body, true, "upload-pack ls-refs")
	if err != nil {
		return nil, "", err
	}
	return decodeV2LSRefs(bytes.NewReader(data))
}

func decodeV2LSRefs(r *bytes.Reader) ([]*plumbing.Reference, plumbing.ReferenceName, error) {
	reader := NewPacketReader(r)
	var refs []*plumbing.Reference
	var headTarget plumbing.ReferenceName
	for {
		kind, payload, err := reader.ReadPacket()
		if err != nil {
			return nil, "", err
		}
		if kind == PacketFlush {
			return refs, headTarget, nil
		}
		if kind != PacketData {
			return nil, "", fmt.Errorf("unexpected packet type %v in ls-refs response", kind)
		}
		fields := strings.Fields(strings.TrimSpace(string(payload)))
		if len(fields) < 2 {
			return nil, "", fmt.Errorf("malformed ls-refs response line %q", payload)
		}
		if fields[0] == "unborn" {
			continue
		}
		hash := plumbing.NewHash(fields[0])
		name := plumbing.ReferenceName(fields[1])
		if name == plumbing.HEAD {
			// HEAD is surfaced via headTarget only; not appended to the ref
			// slice because it is a symbolic ref, matching v1 behavior where
			// symrefs are filtered out by downstream RefHashMap.
			for _, attr := range fields[2:] {
				if target, ok := strings.CutPrefix(attr, "symref-target:"); ok {
					headTarget = plumbing.ReferenceName(target)
					break
				}
			}
			continue
		}
		refs = append(refs, plumbing.NewHashReference(name, hash))
	}
}

// headTargetFromAdv extracts the branch HEAD points to from v1 advertised
// capabilities. Returns empty when HEAD is detached or no symref is advertised.
func headTargetFromAdv(adv *packp.AdvRefs) plumbing.ReferenceName {
	if adv == nil || adv.Capabilities == nil {
		return ""
	}
	for _, value := range adv.Capabilities.Get(capability.SymRef) {
		parts := strings.SplitN(value, ":", 2)
		if len(parts) != 2 {
			continue
		}
		if plumbing.ReferenceName(parts[0]) == plumbing.HEAD {
			return plumbing.ReferenceName(parts[1])
		}
	}
	return ""
}

func decodeV1AdvRefs(data []byte) (*packp.AdvRefs, error) {
	rd := bufio.NewReader(bytes.NewReader(data))
	consumedSmartHeader, err := consumeSmartInfoRefsHeader(rd)
	if err != nil {
		return nil, fmt.Errorf("%w; body-prefix=%q", err, bodyPreview(data))
	}
	if consumedSmartHeader {
		if _, err := rd.Peek(1); errors.Is(err, io.EOF) {
			return nil, transport.ErrEmptyRemoteRepository
		}
	}

	ar := packp.NewAdvRefs()
	if err := ar.Decode(rd); err != nil {
		if errors.Is(err, packp.ErrEmptyAdvRefs) {
			return nil, transport.ErrEmptyRemoteRepository
		}
		return nil, fmt.Errorf("%w; body-prefix=%q", err, bodyPreview(data))
	}
	return ar, nil
}

func consumeSmartInfoRefsHeader(rd *bufio.Reader) (bool, error) {
	_, prefix, err := pktline.PeekLine(rd)
	if err != nil {
		return false, fmt.Errorf("peek pktline: %w", err)
	}
	if !bytes.HasPrefix(prefix, []byte("# service=")) {
		return false, nil
	}

	var reply packp.SmartReply
	if err := reply.Decode(rd); err != nil {
		return true, fmt.Errorf("decode smart reply: %w", err)
	}
	if reply.Service == "" {
		return true, errors.New("missing smart HTTP service name")
	}
	return true, nil
}

func bodyPreview(data []byte) string {
	if len(data) == 0 {
		return ""
	}
	limit := 200
	if len(data) < limit {
		limit = len(data)
	}
	preview := string(data[:limit])
	preview = strings.ReplaceAll(preview, "\n", `\n`)
	preview = strings.ReplaceAll(preview, "\r", `\r`)
	if len(data) > limit {
		preview += "..."
	}
	return preview
}

// RefHashMap converts a reference slice to a map of name→hash.
func RefHashMap(refs []*plumbing.Reference) map[plumbing.ReferenceName]plumbing.Hash {
	out := make(map[plumbing.ReferenceName]plumbing.Hash, len(refs))
	for _, ref := range refs {
		if ref.Type() == plumbing.HashReference {
			out[ref.Name()] = ref.Hash()
		}
	}
	return out
}
