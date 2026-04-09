package syncer

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/plumbing/format/pktline"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/sideband"
	"github.com/go-git/go-git/v5/plumbing/transport"
	transporthttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/go-git/go-git/v5/utils/ioutil"
)

const (
	delimPkt       = "0001"
	responseEndPkt = "0002"
	statsPhaseHdr  = "X-Git-Sync-Stats-Phase"
)

type packetType int

const (
	packetTypeData packetType = iota
	packetTypeFlush
	packetTypeDelim
	packetTypeResponseEnd
)

type v2CapabilityAdvertisement struct {
	Capabilities map[string]string
}

func (a *v2CapabilityAdvertisement) Supports(name string) bool {
	if a == nil {
		return false
	}
	_, ok := a.Capabilities[name]
	return ok
}

func (a *v2CapabilityAdvertisement) Value(name string) string {
	if a == nil {
		return ""
	}
	return a.Capabilities[name]
}

type packetReader struct {
	r *bufio.Reader
}

func newPacketReader(r io.Reader) *packetReader {
	if br, ok := r.(*bufio.Reader); ok {
		return &packetReader{r: br}
	}
	return &packetReader{r: bufio.NewReader(r)}
}

func (r *packetReader) Reader() *bufio.Reader {
	return r.r
}

func (r *packetReader) ReadPacket() (packetType, []byte, error) {
	header := make([]byte, 4)
	if _, err := io.ReadFull(r.r, header); err != nil {
		return packetTypeData, nil, err
	}

	switch string(header) {
	case "0000":
		return packetTypeFlush, nil, nil
	case delimPkt:
		return packetTypeDelim, nil, nil
	case responseEndPkt:
		return packetTypeResponseEnd, nil, nil
	}

	var headerArr [4]byte
	copy(headerArr[:], header)
	n, err := pktlineLength(headerArr)
	if err != nil {
		return packetTypeData, nil, err
	}
	if n <= 4 {
		return packetTypeData, nil, pktline.ErrInvalidPktLen
	}

	payload := make([]byte, n-4)
	if _, err := io.ReadFull(r.r, payload); err != nil {
		return packetTypeData, nil, err
	}
	return packetTypeData, payload, nil
}

func pktlineLength(header [4]byte) (int, error) {
	var n int
	for _, b := range header {
		value, err := asciiHexToByte(b)
		if err != nil {
			return 0, pktline.ErrInvalidPktLen
		}
		n = 16*n + int(value)
	}
	return n, nil
}

func asciiHexToByte(b byte) (byte, error) {
	switch {
	case b >= '0' && b <= '9':
		return b - '0', nil
	case b >= 'a' && b <= 'f':
		return b - 'a' + 10, nil
	case b >= 'A' && b <= 'F':
		return b - 'A' + 10, nil
	default:
		return 0, pktline.ErrInvalidPktLen
	}
}

func decodeV2CapabilityAdvertisement(r io.Reader) (*v2CapabilityAdvertisement, error) {
	reader := newPacketReader(r)

	var (
		kind    packetType
		payload []byte
		err     error
	)
	for {
		kind, payload, err = reader.ReadPacket()
		if err != nil {
			return nil, err
		}
		if kind == packetTypeFlush {
			continue
		}
		if kind != packetTypeData {
			return nil, fmt.Errorf("unexpected packet type %v before protocol advertisement", kind)
		}
		if strings.HasPrefix(string(payload), "# service=") {
			continue
		}
		if string(payload) != "version 2\n" {
			return nil, fmt.Errorf("unexpected protocol advertisement %q", payload)
		}
		break
	}

	adv := &v2CapabilityAdvertisement{Capabilities: map[string]string{}}
	for {
		kind, payload, err = reader.ReadPacket()
		if err != nil {
			return nil, err
		}
		if kind == packetTypeFlush {
			return adv, nil
		}
		if kind != packetTypeData {
			return nil, fmt.Errorf("unexpected packet type %v in capability advertisement", kind)
		}

		line := strings.TrimSuffix(string(payload), "\n")
		name, value, _ := strings.Cut(line, "=")
		adv.Capabilities[name] = value
	}
}

func encodeV2CommandRequest(command string, capabilityArgs []string, commandArgs []string) ([]byte, error) {
	var buf bytes.Buffer
	enc := pktline.NewEncoder(&buf)
	if err := enc.EncodeString("command=" + command + "\n"); err != nil {
		return nil, err
	}
	for _, arg := range capabilityArgs {
		if err := enc.EncodeString(arg + "\n"); err != nil {
			return nil, err
		}
	}
	if len(commandArgs) > 0 {
		if _, err := buf.WriteString(delimPkt); err != nil {
			return nil, err
		}
		for _, arg := range commandArgs {
			if err := enc.EncodeString(arg + "\n"); err != nil {
				return nil, err
			}
		}
	}
	if err := enc.Flush(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

type sourceRefService struct {
	protocol string
	v1       *packp.AdvRefs
	v2       *v2CapabilityAdvertisement
}

func listSourceRefs(ctx context.Context, conn *transportConn, cfg Config) ([]*plumbing.Reference, *sourceRefService, error) {
	switch cfg.ProtocolMode {
	case protocolModeV1:
		adv, refs, err := listSourceRefsV1(ctx, conn)
		if err != nil {
			return nil, nil, err
		}
		return refs, &sourceRefService{protocol: protocolModeV1, v1: adv}, nil
	case protocolModeAuto, protocolModeV2:
		data, err := requestInfoRefs(ctx, conn, transport.UploadPackServiceName, "version=2")
		if err != nil {
			return nil, nil, err
		}

		if adv, err := decodeV2CapabilityAdvertisement(bytes.NewReader(data)); err == nil {
			if !adv.Supports("ls-refs") || !adv.Supports("fetch") {
				return nil, nil, fmt.Errorf("source does not advertise required protocol v2 commands")
			}
			refs, err := listSourceRefsV2(ctx, conn, adv, cfg)
			if err != nil {
				return nil, nil, err
			}
			return refs, &sourceRefService{protocol: protocolModeV2, v2: adv}, nil
		}

		if cfg.ProtocolMode == protocolModeV2 {
			return nil, nil, fmt.Errorf("source did not negotiate protocol v2")
		}

		adv, err := decodeV1AdvertisedRefs(data)
		if err != nil {
			return nil, nil, err
		}
		refs, err := advertisedReferences(adv)
		if err != nil {
			return nil, nil, err
		}
		return refs, &sourceRefService{protocol: protocolModeV1, v1: adv}, nil
	default:
		return nil, nil, fmt.Errorf("unsupported protocol mode %q", cfg.ProtocolMode)
	}
}

func (s *sourceRefService) Fetch(ctx context.Context, repo *git.Repository, conn *transportConn, desired map[plumbing.ReferenceName]desiredRef, targetRefs map[plumbing.ReferenceName]plumbing.Hash) error {
	switch s.protocol {
	case protocolModeV2:
		return fetchSourceRefsV2(ctx, repo, conn, s.v2, desired, targetRefs)
	case protocolModeV1:
		return fetchSourceRefsWithHavesV1(ctx, repo, conn, s.v1, desired, targetRefs)
	default:
		return fmt.Errorf("unsupported source protocol %q", s.protocol)
	}
}

func (s *sourceRefService) FetchPack(ctx context.Context, conn *transportConn, desired map[plumbing.ReferenceName]desiredRef, targetRefs map[plumbing.ReferenceName]plumbing.Hash) (io.ReadCloser, error) {
	switch s.protocol {
	case protocolModeV2:
		return fetchSourcePackV2(ctx, conn, s.v2, desired, targetRefs)
	case protocolModeV1:
		return fetchSourcePackV1(ctx, conn, s.v1, desired, targetRefs)
	default:
		return nil, fmt.Errorf("unsupported source protocol %q", s.protocol)
	}
}

func sourceCapabilities(s *sourceRefService) []string {
	switch s.protocol {
	case protocolModeV2:
		keys := make([]string, 0, len(s.v2.Capabilities))
		for key, value := range s.v2.Capabilities {
			if value == "" {
				keys = append(keys, key)
				continue
			}
			keys = append(keys, key+"="+value)
		}
		sort.Strings(keys)
		return keys
	case protocolModeV1:
		if s.v1 == nil || s.v1.Capabilities == nil {
			return nil
		}
		all := s.v1.Capabilities.All()
		items := make([]string, 0, len(all))
		for _, cap := range all {
			values := s.v1.Capabilities.Get(cap)
			if len(values) == 0 {
				items = append(items, string(cap))
				continue
			}
			for _, value := range values {
				items = append(items, string(cap)+"="+value)
			}
		}
		sort.Strings(items)
		return items
	default:
		return nil
	}
}

func advCapabilities(adv *packp.AdvRefs) []string {
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
	sort.Strings(items)
	return items
}

func listSourceRefsV1(ctx context.Context, conn *transportConn) (*packp.AdvRefs, []*plumbing.Reference, error) {
	adv, err := advertisedRefsV1(ctx, conn, transport.UploadPackServiceName)
	if err != nil {
		return nil, nil, err
	}
	refs, err := advertisedReferences(adv)
	if err != nil {
		return nil, nil, err
	}
	return adv, refs, nil
}

func listSourceRefsV2(ctx context.Context, conn *transportConn, adv *v2CapabilityAdvertisement, cfg Config) ([]*plumbing.Reference, error) {
	args := []string{"peel"}
	for _, prefix := range sourceRefPrefixes(cfg) {
		args = append(args, "ref-prefix "+prefix)
	}

	body, err := encodeV2CommandRequest("ls-refs", v2RequestCapabilities(adv), args)
	if err != nil {
		return nil, err
	}

	data, err := postRPCWithPhase(ctx, conn, transport.UploadPackServiceName, body, true, "upload-pack ls-refs")
	if err != nil {
		return nil, err
	}
	return decodeV2LSRefs(bytes.NewReader(data))
}

func fetchSourceRefsV2(
	ctx context.Context,
	repo *git.Repository,
	conn *transportConn,
	adv *v2CapabilityAdvertisement,
	desired map[plumbing.ReferenceName]desiredRef,
	targetRefs map[plumbing.ReferenceName]plumbing.Hash,
) error {
	wants := make([]plumbing.Hash, 0, len(desired))
	for _, ref := range desired {
		wants = append(wants, ref.SourceHash)
	}
	wants = sortedUniqueHashes(wants)
	haves := sortedUniqueHashes(mapsRefValues(targetRefs))
	if len(wants) == 0 {
		return git.NoErrAlreadyUpToDate
	}

	commandArgs := make([]string, 0, len(wants)+len(haves)+4)
	commandArgs = append(commandArgs, "ofs-delta", "no-progress")
	for _, hash := range wants {
		commandArgs = append(commandArgs, "want "+hash.String())
	}
	for _, hash := range haves {
		commandArgs = append(commandArgs, "have "+hash.String())
	}
	commandArgs = append(commandArgs, "done")
	conn.stats.addWantsHaves("source upload-pack", len(wants), len(haves))

	body, err := encodeV2CommandRequest("fetch", v2RequestCapabilities(adv), commandArgs)
	if err != nil {
		return err
	}

	reader, err := postRPCStreamWithPhase(ctx, conn, transport.UploadPackServiceName, body, true, "upload-pack fetch")
	if err != nil {
		return err
	}
	defer ioutil.CheckClose(reader, &err)

	if err := storeV2FetchPack(repo, reader); err != nil {
		return err
	}

	return storeFetchedSourceRefs(repo, desired)
}

func fetchSourceCommitGraphV2(
	ctx context.Context,
	repo *git.Repository,
	conn *transportConn,
	adv *v2CapabilityAdvertisement,
	ref desiredRef,
) error {
	if !fetchCapabilitySupports(adv, "filter") {
		return fmt.Errorf("source does not advertise fetch filter support")
	}

	commandArgs := []string{
		"ofs-delta",
		"no-progress",
		"filter tree:0",
		"want " + ref.SourceHash.String(),
		"done",
	}
	conn.stats.addWantsHaves("source upload-pack", 1, 0)

	body, err := encodeV2CommandRequest("fetch", v2RequestCapabilities(adv), commandArgs)
	if err != nil {
		return err
	}

	reader, err := postRPCStreamWithPhase(ctx, conn, transport.UploadPackServiceName, body, true, "upload-pack fetch")
	if err != nil {
		return err
	}
	defer ioutil.CheckClose(reader, &err)

	if err := storeV2FetchPack(repo, reader); err != nil {
		return err
	}
	return storeFetchedSourceRefs(repo, singleDesiredRef(ref.SourceRef, ref.TargetRef, ref.SourceHash))
}

func fetchSourcePackV2(
	ctx context.Context,
	conn *transportConn,
	adv *v2CapabilityAdvertisement,
	desired map[plumbing.ReferenceName]desiredRef,
	targetRefs map[plumbing.ReferenceName]plumbing.Hash,
) (io.ReadCloser, error) {
	body, wants, haves, err := sourceFetchRequestV2(adv, desired, targetRefs)
	if err != nil {
		return nil, err
	}
	if wants == 0 {
		return nil, git.NoErrAlreadyUpToDate
	}
	conn.stats.addWantsHaves("source upload-pack", wants, haves)

	reader, err := postRPCStreamWithPhase(ctx, conn, transport.UploadPackServiceName, body, true, "upload-pack fetch")
	if err != nil {
		return nil, err
	}
	packReader, err := openV2FetchPackStream(reader)
	if err != nil {
		_ = reader.Close()
		return nil, err
	}
	return packReader, nil
}

func storeV2FetchPack(repo *git.Repository, r io.Reader) error {
	reader := newPacketReader(r)
	for {
		kind, payload, err := reader.ReadPacket()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("decode protocol v2 fetch response: %w", err)
		}

		switch kind {
		case packetTypeFlush:
			return nil
		case packetTypeDelim, packetTypeResponseEnd:
			continue
		case packetTypeData:
			line := string(payload)
			switch line {
			case "packfile\n":
				demux := sideband.NewDemuxer(sideband.Sideband64k, reader.Reader())
				if err := packfile.UpdateObjectStorage(repo.Storer, demux); err != nil {
					return fmt.Errorf("store source packfile: %w", err)
				}
				return nil
			case "acknowledgments\n", "shallow-info\n":
				if err := skipV2Section(reader); err != nil {
					return err
				}
			default:
				return fmt.Errorf("unexpected protocol v2 fetch section %q", strings.TrimSpace(line))
			}
		}
	}
}

func openV2FetchPackStream(body io.ReadCloser) (io.ReadCloser, error) {
	reader := newPacketReader(body)
	for {
		kind, payload, err := reader.ReadPacket()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil, io.ErrUnexpectedEOF
			}
			return nil, fmt.Errorf("decode protocol v2 fetch response: %w", err)
		}

		switch kind {
		case packetTypeFlush:
			return nil, io.ErrUnexpectedEOF
		case packetTypeDelim, packetTypeResponseEnd:
			continue
		case packetTypeData:
			line := string(payload)
			switch line {
			case "packfile\n":
				return &wrappedReadCloser{
					Reader: sideband.NewDemuxer(sideband.Sideband64k, reader.Reader()),
					Closer: body,
				}, nil
			case "acknowledgments\n", "shallow-info\n":
				if err := skipV2Section(reader); err != nil {
					return nil, err
				}
			default:
				return nil, fmt.Errorf("unexpected protocol v2 fetch section %q", strings.TrimSpace(line))
			}
		}
	}
}

func skipV2Section(reader *packetReader) error {
	for {
		kind, _, err := reader.ReadPacket()
		if err != nil {
			return err
		}
		if kind == packetTypeDelim || kind == packetTypeFlush {
			return nil
		}
	}
}

func decodeV2LSRefs(r io.Reader) ([]*plumbing.Reference, error) {
	reader := newPacketReader(r)
	var refs []*plumbing.Reference
	for {
		kind, payload, err := reader.ReadPacket()
		if err != nil {
			return nil, err
		}
		if kind == packetTypeFlush {
			return refs, nil
		}
		if kind != packetTypeData {
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

func sourceRefPrefixes(cfg Config) []string {
	prefixSet := map[string]struct{}{}

	addPrefix := func(ref plumbing.ReferenceName) {
		switch {
		case ref.IsBranch():
			prefixSet["refs/heads/"] = struct{}{}
		case ref.IsTag():
			prefixSet["refs/tags/"] = struct{}{}
		}
	}

	if len(cfg.Mappings) > 0 {
		for _, mapping := range cfg.Mappings {
			sourceRef, _, _, err := normalizeMapping(mapping)
			if err != nil {
				continue
			}
			addPrefix(sourceRef)
		}
	} else {
		prefixSet["refs/heads/"] = struct{}{}
	}
	if cfg.IncludeTags {
		prefixSet["refs/tags/"] = struct{}{}
	}

	prefixes := make([]string, 0, len(prefixSet))
	for prefix := range prefixSet {
		prefixes = append(prefixes, prefix)
	}
	sort.Strings(prefixes)
	return prefixes
}

func fetchCapabilitySupports(adv *v2CapabilityAdvertisement, feature string) bool {
	if adv == nil {
		return false
	}
	values := strings.Fields(adv.Value("fetch"))
	for _, value := range values {
		if value == feature {
			return true
		}
	}
	return false
}

func v2RequestCapabilities(adv *v2CapabilityAdvertisement) []string {
	var caps []string
	if agent := adv.Value("agent"); agent != "" {
		caps = append(caps, "agent="+capability.DefaultAgent())
	}
	return caps
}

func sourceFetchRequestV2(
	adv *v2CapabilityAdvertisement,
	desired map[plumbing.ReferenceName]desiredRef,
	targetRefs map[plumbing.ReferenceName]plumbing.Hash,
) ([]byte, int, int, error) {
	wants := make([]plumbing.Hash, 0, len(desired))
	for _, ref := range desired {
		wants = append(wants, ref.SourceHash)
	}
	wants = sortedUniqueHashes(wants)
	haves := sortedUniqueHashes(mapsRefValues(targetRefs))
	if len(wants) == 0 {
		return nil, 0, 0, nil
	}

	commandArgs := make([]string, 0, len(wants)+len(haves)+4)
	commandArgs = append(commandArgs, "ofs-delta", "no-progress")
	for _, hash := range wants {
		commandArgs = append(commandArgs, "want "+hash.String())
	}
	for _, hash := range haves {
		commandArgs = append(commandArgs, "have "+hash.String())
	}
	commandArgs = append(commandArgs, "done")

	body, err := encodeV2CommandRequest("fetch", v2RequestCapabilities(adv), commandArgs)
	if err != nil {
		return nil, 0, 0, err
	}
	return body, len(wants), len(haves), nil
}

type wrappedReadCloser struct {
	io.Reader
	io.Closer
}

func storeFetchedSourceRefs(repo *git.Repository, desired map[plumbing.ReferenceName]desiredRef) error {
	for _, ref := range desired {
		localRef := plumbing.ReferenceName(localBranchRef(sourceRemoteName, ref.TargetRef.Short()))
		if ref.Kind == RefKindTag {
			localRef = plumbing.ReferenceName("refs/remotes/" + sourceRemoteName + "/tags/" + ref.TargetRef.Short())
		}
		if err := repo.Storer.SetReference(plumbing.NewHashReference(localRef, ref.SourceHash)); err != nil {
			return fmt.Errorf("set local source ref %s: %w", ref.SourceRef, err)
		}
	}
	return nil
}

func requestInfoRefs(ctx context.Context, conn *transportConn, service, gitProtocol string) ([]byte, error) {
	url := fmt.Sprintf("%s/info/refs?service=%s", conn.endpoint.String(), service)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "*/*")
	req.Header.Set("User-Agent", capability.DefaultAgent())
	req.Header.Set(statsPhaseHdr, service+" info-refs")
	if gitProtocol != "" {
		req.Header.Set("Git-Protocol", gitProtocol)
	}
	applyAuth(req, conn.authMethod())

	res, err := conn.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if err := transporthttp.NewErr(res); err != nil {
		return nil, err
	}
	return io.ReadAll(res.Body)
}

func advertisedRefsV1(ctx context.Context, conn *transportConn, service string) (*packp.AdvRefs, error) {
	data, err := requestInfoRefs(ctx, conn, service, "")
	if err != nil {
		return nil, err
	}
	return decodeV1AdvertisedRefs(data)
}

func decodeV1AdvertisedRefs(data []byte) (*packp.AdvRefs, error) {
	ar := packp.NewAdvRefs()
	if err := ar.Decode(bytes.NewReader(data)); err != nil {
		if err == packp.ErrEmptyAdvRefs {
			return nil, transport.ErrEmptyRemoteRepository
		}
		return nil, err
	}
	return ar, nil
}

func postRPC(ctx context.Context, conn *transportConn, service string, body []byte, gitProtocolV2 bool) ([]byte, error) {
	reader, err := postRPCStream(ctx, conn, service, body, gitProtocolV2)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	return io.ReadAll(reader)
}

func postRPCWithPhase(ctx context.Context, conn *transportConn, service string, body []byte, gitProtocolV2 bool, phase string) ([]byte, error) {
	reader, err := postRPCStreamWithPhase(ctx, conn, service, body, gitProtocolV2, phase)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	return io.ReadAll(reader)
}

func postRPCStream(ctx context.Context, conn *transportConn, service string, body []byte, gitProtocolV2 bool) (io.ReadCloser, error) {
	return postRPCStreamWithPhase(ctx, conn, service, body, gitProtocolV2, service)
}

func postRPCStreamWithPhase(ctx context.Context, conn *transportConn, service string, body []byte, gitProtocolV2 bool, phase string) (io.ReadCloser, error) {
	url := fmt.Sprintf("%s/%s", conn.endpoint.String(), service)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", fmt.Sprintf("application/x-%s-request", service))
	req.Header.Set("Accept", fmt.Sprintf("application/x-%s-result", service))
	req.Header.Set("User-Agent", capability.DefaultAgent())
	req.Header.Set(statsPhaseHdr, phase)
	if gitProtocolV2 {
		req.Header.Set("Git-Protocol", "version=2")
	}
	applyAuth(req, conn.authMethod())

	res, err := conn.http.Do(req)
	if err != nil {
		return nil, err
	}
	if err := transporthttp.NewErr(res); err != nil {
		_ = res.Body.Close()
		return nil, err
	}
	return res.Body, nil
}
