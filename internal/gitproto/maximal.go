package gitproto

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/transport"
)

// MaximalFeature is entiredb's ls-refs extension. It is the capability
// feature, the request argument, and the ref attribute. A server that
// receives it marks each listed ref whose commit no other listed ref reaches.
// It leaves a ref unmarked only when it has proved that another listed ref's
// commit reaches it, so the marked refs imply the history of every listed ref.
const MaximalFeature = "entire-maximal"

// ErrMaximalUnsupported reports that a server does not advertise
// MaximalFeature over protocol v2.
var ErrMaximalUnsupported = errors.New("server does not advertise " + MaximalFeature)

// ErrMaximalInconsistent reports a MaximalFeature listing that breaks the
// server's own rules: a ref listed twice, two refs at the same object marked
// differently, or a non-empty listing with no mark at all. Every non-empty set
// of refs has a maximal one, so a listing like that cannot be trusted to drop
// any have.
var ErrMaximalInconsistent = errors.New("inconsistent " + MaximalFeature + " listing")

// MaximalTips is a target's ls-refs listing with the MaximalFeature marks.
// Reduce uses it to drop redundant haves.
type MaximalTips struct {
	marked   map[plumbing.ReferenceName]plumbing.Hash
	unmarked map[plumbing.ReferenceName]plumbing.Hash
}

// Marked and Unmarked report the listing's size, for logs and tests.
func (m *MaximalTips) Marked() int   { return len(m.marked) }
func (m *MaximalTips) Unmarked() int { return len(m.unmarked) }

// ListMaximalTips lists conn's upload-pack refs under prefixes with the
// MaximalFeature marks. It returns ErrMaximalUnsupported, before sending
// ls-refs, when the server does not speak protocol v2 or, unless
// assumeSupported is set, does not advertise the feature. Protocol v2 forbids
// an argument the server did not advertise, so set assumeSupported only for a
// server known to accept it; one that does not fails the ls-refs request.
//
// List only refs the source also holds. The source ignores a have it lacks,
// and a marked tip it lacks hides the unmarked refs behind it, so the fetch
// carries history the target already has. That costs bytes, never
// correctness.
func ListMaximalTips(ctx context.Context, conn Conn, prefixes []string, assumeSupported bool) (*MaximalTips, error) {
	data, err := RequestInfoRefs(ctx, conn, transport.UploadPackService, GitProtocolV2)
	if err != nil {
		return nil, err
	}
	caps, err := DecodeV2Capabilities(bytes.NewReader(data))
	if err != nil {
		// A v1 advertisement: the server did not negotiate v2.
		return nil, ErrMaximalUnsupported
	}
	if !caps.Supports("ls-refs") || (!assumeSupported && !caps.LSRefsSupports(MaximalFeature)) {
		return nil, ErrMaximalUnsupported
	}
	args := []string{MaximalFeature}
	for _, prefix := range prefixes {
		args = append(args, "ref-prefix "+prefix)
	}
	body, err := EncodeCommand("ls-refs", caps.RequestCapabilities(), args)
	if err != nil {
		return nil, err
	}
	resp, err := PostRPC(ctx, conn, transport.UploadPackService, body, true, "upload-pack ls-refs "+MaximalFeature)
	if err != nil {
		return nil, err
	}
	return decodeMaximalLSRefs(bytes.NewReader(resp))
}

func decodeMaximalLSRefs(r *bytes.Reader) (*MaximalTips, error) {
	m := &MaximalTips{
		marked:   map[plumbing.ReferenceName]plumbing.Hash{},
		unmarked: map[plumbing.ReferenceName]plumbing.Hash{},
	}
	// The server marks by peeled object, so every ref at one object carries
	// the same mark.
	markOf := map[plumbing.Hash]bool{}
	reader := NewPacketReader(r)
	for {
		kind, payload, err := reader.ReadPacket()
		if err != nil {
			return nil, err
		}
		if kind == PacketFlush {
			if len(m.unmarked) > 0 && len(m.marked) == 0 {
				return nil, fmt.Errorf("%w: %d refs, none marked", ErrMaximalInconsistent, len(m.unmarked))
			}
			return m, nil
		}
		if kind != PacketData {
			return nil, fmt.Errorf("unexpected packet type %v in ls-refs response", kind)
		}
		fields := strings.Fields(strings.TrimSpace(string(payload)))
		if len(fields) < 2 {
			return nil, fmt.Errorf("malformed ls-refs response line %q", payload)
		}
		name := plumbing.ReferenceName(fields[1])
		// HEAD is not a have: the branch it names carries the same mark.
		if fields[0] == "unborn" || name == plumbing.HEAD {
			continue
		}
		if !plumbing.IsHash(fields[0]) {
			return nil, fmt.Errorf("malformed ls-refs object id %q", fields[0])
		}
		hash := plumbing.NewHash(fields[0])
		identity := hash
		marked := false
		for _, attr := range fields[2:] {
			if attr == MaximalFeature {
				marked = true
			} else if peeled, ok := strings.CutPrefix(attr, "peeled:"); ok && plumbing.IsHash(peeled) {
				identity = plumbing.NewHash(peeled)
			}
		}
		if _, dup := m.marked[name]; dup {
			return nil, fmt.Errorf("%w: %s listed twice", ErrMaximalInconsistent, name)
		}
		if _, dup := m.unmarked[name]; dup {
			return nil, fmt.Errorf("%w: %s listed twice", ErrMaximalInconsistent, name)
		}
		if prev, ok := markOf[identity]; ok && prev != marked {
			return nil, fmt.Errorf("%w: refs at %s marked differently", ErrMaximalInconsistent, identity)
		}
		markOf[identity] = marked
		if marked {
			m.marked[name] = hash
		} else {
			m.unmarked[name] = hash
		}
	}
}

// Reduce returns haves without the refs the listing proved redundant. A nil
// receiver returns haves unchanged. keep, when non-nil, names refs Reduce must
// never drop, for a caller that filters those refs by rules of its own.
//
// An unmarked ref is implied only through a marked ref that reaches it, and
// the listing does not say which one. So Reduce drops anything only when every
// marked ref is in haves at its listed hash. A ref that moved since the
// listing, such as one this run pushed, keeps the whole map unreduced. An
// unmarked ref is dropped only at its listed hash. Refs the listing did not
// cover stay.
func (m *MaximalTips) Reduce(
	haves map[plumbing.ReferenceName]plumbing.Hash,
	keep func(plumbing.ReferenceName) bool,
) map[plumbing.ReferenceName]plumbing.Hash {
	// No marks proves nothing; decodeMaximalLSRefs already rejects that
	// listing, and this keeps a hand-built one from dropping every have.
	if m == nil || len(m.unmarked) == 0 || len(m.marked) == 0 {
		return haves
	}
	for name, hash := range m.marked {
		if got, ok := haves[name]; !ok || got != hash {
			return haves
		}
	}
	out := make(map[plumbing.ReferenceName]plumbing.Hash, len(haves))
	for name, hash := range haves {
		if listed, ok := m.unmarked[name]; ok && listed == hash && (keep == nil || !keep(name)) {
			continue
		}
		out[name] = hash
	}
	return out
}
