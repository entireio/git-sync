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
	"github.com/go-git/go-git/v6/plumbing/protocol/capability"
	"github.com/go-git/go-git/v6/plumbing/protocol/packp"
	"github.com/go-git/go-git/v6/plumbing/transport"
)

// GitProtocolV2 is the value passed via the Git-Protocol header / GIT_PROTOCOL
// env var to negotiate protocol v2 with the remote.
const GitProtocolV2 = "version=2"

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
	// HeadUnborn is true when the source reported HEAD as unborn — its
	// symref target does not exist. That is ALL it means. It is emphatically
	// NOT a statement that the repository is empty: git emits the unborn line
	// for any dangling HEAD, so a repository holding refs/heads/other while
	// HEAD points at a never-created refs/heads/main reports unborn too, and
	// hiding (uploadpack.hideRefs) can reduce that repository's whole
	// advertisement to the unborn line alone. Verified against git 2.53.
	//
	// It is therefore a NECESSARY condition for emptiness and never a
	// sufficient one: a truly empty repository always reports unborn, so its
	// absence is a reliable disqualifier, but its presence proves nothing
	// about refs the reader cannot see. A caller that needs to act on
	// emptiness must obtain that from the server's own repository state; use
	// this only to cross-check such an assertion.
	//
	// False means "not reported", NOT "the source has commits": a v1 source,
	// or a v2 source that does not advertise ls-refs=unborn, leaves it false
	// however empty it is.
	HeadUnborn bool

	// SkippedRefNames are advertised ref names dropped as invalid (see
	// PartitionRefNames). Surfaced because their absence is load-bearing for
	// any caller reasoning about an empty ref set: names dropped here leave
	// refs empty while the repository plainly has some, which is
	// indistinguishable from emptiness by ref count alone.
	SkippedRefNames []string
}

// ListSourceRefs discovers refs from the source using the configured protocol mode.
// Returns the list of refs and a RefService for subsequent operations.
func ListSourceRefs(ctx context.Context, conn Conn, protocolMode string, refPrefixes []string) ([]*plumbing.Reference, *RefService, error) {
	switch protocolMode {
	case "v1":
		adv, refs, err := listSourceRefsV1(ctx, conn)
		if err != nil {
			return nil, nil, err
		}
		return refs, &RefService{Protocol: "v1", V1Adv: adv, HeadTarget: headTargetFromAdv(adv)}, nil

	case "auto", "v2":
		data, err := RequestInfoRefs(ctx, conn, transport.UploadPackService, GitProtocolV2)
		if err != nil {
			if protocolMode == "auto" && isSSHScheme(conn) {
				refs, svc, v1Err := listSourceRefsAutoV1(ctx, conn)
				if v1Err != nil {
					return nil, nil, errors.Join(err, v1Err)
				}
				return refs, svc, nil
			}
			return nil, nil, err
		}
		if caps, err := DecodeV2Capabilities(bytes.NewReader(data)); err == nil {
			if !caps.Supports("ls-refs") || !caps.Supports("fetch") {
				return nil, nil, errors.New("source does not advertise required protocol v2 commands")
			}
			refs, headTarget, unborn, skipped, err := listSourceRefsV2(ctx, conn, caps, refPrefixes)
			if err != nil {
				return nil, nil, err
			}
			return refs, &RefService{Protocol: "v2", V2Caps: caps, HeadTarget: headTarget, HeadUnborn: unborn, SkippedRefNames: skipped}, nil
		}
		if protocolMode == "v2" {
			return nil, nil, errors.New("source did not negotiate protocol v2")
		}
		// Fall back to v1
		adv, err := decodeV1AdvRefs(data)
		if err != nil {
			return nil, nil, err
		}
		refs, skipped, err := AdvRefsToSlice(adv)
		if err != nil {
			return nil, nil, err
		}
		WarnSkippedRefNames(conn.ProgressWriter(), "source", skipped)
		return refs, &RefService{Protocol: "v1", V1Adv: adv, HeadTarget: headTargetFromAdv(adv)}, nil

	default:
		return nil, nil, fmt.Errorf("unsupported protocol mode %q", protocolMode)
	}
}

func listSourceRefsAutoV1(ctx context.Context, conn Conn) ([]*plumbing.Reference, *RefService, error) {
	adv, refs, err := listSourceRefsV1(ctx, conn)
	if err != nil {
		return nil, nil, err
	}
	return refs, &RefService{Protocol: "v1", V1Adv: adv, HeadTarget: headTargetFromAdv(adv)}, nil
}

func isSSHScheme(conn Conn) bool {
	if conn == nil || conn.Endpoint() == nil {
		return false
	}
	switch conn.Endpoint().Scheme {
	case "ssh", "git+ssh":
		return true
	}
	return false
}

// AdvertisedRefsV1 fetches and decodes v1 advertised refs for the given service.
func AdvertisedRefsV1(ctx context.Context, conn Conn, service string) (*packp.AdvRefs, error) {
	data, err := RequestInfoRefs(ctx, conn, service, "")
	if err != nil {
		return nil, err
	}
	return decodeV1AdvRefs(data)
}

// AdvRefsToSlice converts an AdvRefs to a slice of references.
//
// Peeled tag entries (names ending in "^{}") are dropped: they are
// wire-protocol metadata exposing the commit a tag points to, not real refs.
// Including them in target ref maps causes the planner to schedule a delete
// for a non-existent ref, which receive-pack rejects with HTTP 400
// "invalid reference name".
// Names a remote advertises are untrusted, so anything git would reject is
// dropped here and returned as skipped for the caller to report. Filtering at
// the decode boundary means every path that reads an advertisement is covered
// without each one remembering to ask.
func AdvRefsToSlice(ar *packp.AdvRefs) (refs []*plumbing.Reference, skipped []string, err error) {
	resolved, err := ar.ResolvedReferences()
	if err != nil {
		return nil, nil, fmt.Errorf("resolved references: %w", err)
	}
	out := resolved[:0]
	for _, ref := range resolved {
		if ref.Name().IsPeeled() {
			continue
		}
		out = append(out, ref)
	}
	refs, skipped = PartitionRefNames(out)
	return refs, skipped, nil
}

// AdvRefsCaps returns the sorted capability list from an AdvRefs.
func AdvRefsCaps(adv *packp.AdvRefs) []string {
	if adv == nil || adv.Capabilities.IsEmpty() {
		return nil
	}
	all := adv.Capabilities.All()
	items := make([]string, 0, len(all))
	for _, cap := range all {
		values := adv.Capabilities.Get(cap)
		if len(values) == 0 {
			items = append(items, cap)
			continue
		}
		for _, value := range values {
			items = append(items, cap+"="+value)
		}
	}
	return items
}

func listSourceRefsV1(ctx context.Context, conn Conn) (*packp.AdvRefs, []*plumbing.Reference, error) {
	adv, err := AdvertisedRefsV1(ctx, conn, transport.UploadPackService)
	if err != nil {
		return nil, nil, err
	}
	refs, skipped, err := AdvRefsToSlice(adv)
	if err != nil {
		return nil, nil, err
	}
	WarnSkippedRefNames(conn.ProgressWriter(), "source", skipped)
	return adv, refs, nil
}

func listSourceRefsV2(ctx context.Context, conn Conn, caps *V2Capabilities, prefixes []string) ([]*plumbing.Reference, plumbing.ReferenceName, bool, []string, error) {
	// Always include "HEAD" so the server returns the symref-target attribute
	// for HEAD. Without this, callers that pass only "refs/heads/" or
	// "refs/tags/" prefixes filter HEAD out of the response and lose the
	// default-branch hint that bootstrap planning uses as a trunk cutoff.
	args := []string{"peel", "symrefs", "ref-prefix HEAD"}
	// Ask the server to report an unborn HEAD explicitly, so a repository
	// with no commits says so instead of answering with no ref lines at all.
	// Gated on the advertisement: protocol v2 forbids sending an argument the
	// server did not advertise, and a stricter server may fail the command.
	// Costs nothing — no extra round trip, and a source WITH commits answers
	// exactly as before.
	if caps.LSRefsSupports("unborn") {
		args = append(args, "unborn")
	}
	for _, prefix := range prefixes {
		args = append(args, "ref-prefix "+prefix)
	}
	body, err := EncodeCommand("ls-refs", caps.RequestCapabilities(), args)
	if err != nil {
		return nil, "", false, nil, err
	}
	data, err := PostRPC(ctx, conn, transport.UploadPackService, body, true, "upload-pack ls-refs")
	if err != nil {
		return nil, "", false, nil, err
	}
	refs, headTarget, unborn, skipped, err := decodeV2LSRefs(bytes.NewReader(data))
	if err != nil {
		return nil, "", false, nil, err
	}
	WarnSkippedRefNames(conn.ProgressWriter(), "source", skipped)
	return refs, headTarget, unborn, skipped, nil
}

// decodeV2LSRefs decodes an ls-refs response into its refs, the branch HEAD
// points at, whether the server reported HEAD as unborn, and the ref names
// dropped as invalid for the caller to report.
func decodeV2LSRefs(r *bytes.Reader) ([]*plumbing.Reference, plumbing.ReferenceName, bool, []string, error) {
	reader := NewPacketReader(r)
	var refs []*plumbing.Reference
	var headTarget plumbing.ReferenceName
	var unborn bool
	for {
		kind, payload, err := reader.ReadPacket()
		if err != nil {
			return nil, "", false, nil, err
		}
		if kind == PacketFlush {
			// Names come from the remote, so drop anything git would reject
			// and hand the list back for the caller to report. unborn rides
			// alongside rather than through this filter: it is a property of
			// the repository, not a ref, and the line that carries it is
			// consumed below without ever entering refs.
			valid, skipped := PartitionRefNames(refs)
			return valid, headTarget, unborn, skipped, nil
		}
		if kind != PacketData {
			return nil, "", false, nil, fmt.Errorf("unexpected packet type %v in ls-refs response", kind)
		}
		fields := strings.Fields(strings.TrimSpace(string(payload)))
		if len(fields) < 2 {
			return nil, "", false, nil, fmt.Errorf("malformed ls-refs response line %q", payload)
		}
		if fields[0] == "unborn" {
			// "unborn HEAD symref-target:refs/heads/main": the repository has
			// no commits. Recorded as a flag only — headTarget deliberately
			// stays empty, because consumers treat a non-empty HeadTarget as
			// a branch that exists on the source (mirror-pipeline's
			// default-branch reconcile drives SetDefaultBranch off it), and
			// an unborn target does not.
			if fields[1] == string(plumbing.HEAD) {
				unborn = true
			}
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
	if adv == nil || adv.Capabilities.IsEmpty() {
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

	ar := &packp.AdvRefs{}
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
