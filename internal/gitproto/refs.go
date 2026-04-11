package gitproto

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/transport"
)

// RefService encapsulates the result of source ref discovery and the negotiated
// protocol, providing methods for subsequent fetch and pack operations.
type RefService struct {
	Protocol string // "v1" or "v2"
	V1Adv    *packp.AdvRefs
	V2Caps   *V2Capabilities
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
		return refs, &RefService{Protocol: "v1", V1Adv: adv}, nil

	case "auto", "v2":
		data, err := RequestInfoRefs(ctx, conn, transport.UploadPackServiceName, "version=2")
		if err != nil {
			return nil, nil, err
		}
		if caps, err := DecodeV2Capabilities(bytes.NewReader(data)); err == nil {
			if !caps.Supports("ls-refs") || !caps.Supports("fetch") {
				return nil, nil, fmt.Errorf("source does not advertise required protocol v2 commands")
			}
			refs, err := listSourceRefsV2(ctx, conn, caps, refPrefixes)
			if err != nil {
				return nil, nil, err
			}
			return refs, &RefService{Protocol: "v2", V2Caps: caps}, nil
		}
		if protocolMode == "v2" {
			return nil, nil, fmt.Errorf("source did not negotiate protocol v2")
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
		return refs, &RefService{Protocol: "v1", V1Adv: adv}, nil

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
		return nil, err
	}
	iter, err := refs.IterReferences()
	if err != nil {
		return nil, err
	}
	defer iter.Close()

	var out []*plumbing.Reference
	err = iter.ForEach(func(ref *plumbing.Reference) error {
		out = append(out, ref)
		return nil
	})
	return out, err
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
	adv, err := AdvertisedRefsV1(ctx, conn, transport.UploadPackServiceName)
	if err != nil {
		return nil, nil, err
	}
	refs, err := AdvRefsToSlice(adv)
	if err != nil {
		return nil, nil, err
	}
	return adv, refs, nil
}

func listSourceRefsV2(ctx context.Context, conn *Conn, caps *V2Capabilities, prefixes []string) ([]*plumbing.Reference, error) {
	args := []string{"peel"}
	for _, prefix := range prefixes {
		args = append(args, "ref-prefix "+prefix)
	}
	body, err := EncodeCommand("ls-refs", caps.RequestCapabilities(), args)
	if err != nil {
		return nil, err
	}
	data, err := PostRPC(ctx, conn, transport.UploadPackServiceName, body, true, "upload-pack ls-refs")
	if err != nil {
		return nil, err
	}
	return decodeV2LSRefs(bytes.NewReader(data))
}

func decodeV2LSRefs(r *bytes.Reader) ([]*plumbing.Reference, error) {
	reader := NewPacketReader(r)
	var refs []*plumbing.Reference
	for {
		kind, payload, err := reader.ReadPacket()
		if err != nil {
			return nil, err
		}
		if kind == PacketFlush {
			return refs, nil
		}
		if kind != PacketData {
			return nil, fmt.Errorf("unexpected packet type %v in ls-refs response", kind)
		}
		fields := strings.Fields(strings.TrimSpace(string(payload)))
		if len(fields) < 2 {
			return nil, fmt.Errorf("malformed ls-refs response line %q", payload)
		}
		hash := plumbing.NewHash(fields[0])
		name := plumbing.ReferenceName(fields[1])
		refs = append(refs, plumbing.NewHashReference(name, hash))
	}
}

func decodeV1AdvRefs(data []byte) (*packp.AdvRefs, error) {
	ar := packp.NewAdvRefs()
	if err := ar.Decode(bytes.NewReader(data)); err != nil {
		if err == packp.ErrEmptyAdvRefs {
			return nil, transport.ErrEmptyRemoteRepository
		}
		return nil, err
	}
	return ar, nil
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
