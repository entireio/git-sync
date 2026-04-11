package syncer

import (
	"bytes"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
)

func TestPacketReaderHandlesSpecialPackets(t *testing.T) {
	reader := newPacketReader(bytes.NewBufferString("0000000100020006a\n"))

	kind, payload, err := reader.ReadPacket()
	if err != nil {
		t.Fatalf("read flush: %v", err)
	}
	if kind != packetTypeFlush || payload != nil {
		t.Fatalf("unexpected flush packet: kind=%v payload=%q", kind, payload)
	}

	kind, payload, err = reader.ReadPacket()
	if err != nil {
		t.Fatalf("read delim: %v", err)
	}
	if kind != packetTypeDelim {
		t.Fatalf("unexpected delim kind: %v", kind)
	}

	kind, payload, err = reader.ReadPacket()
	if err != nil {
		t.Fatalf("read response-end: %v", err)
	}
	if kind != packetTypeResponseEnd {
		t.Fatalf("unexpected response-end kind: %v", kind)
	}

	kind, payload, err = reader.ReadPacket()
	if err != nil {
		t.Fatalf("read data: %v", err)
	}
	if kind != packetTypeData || string(payload) != "a\n" {
		t.Fatalf("unexpected data packet: kind=%v payload=%q", kind, payload)
	}
}

func TestDecodeV2CapabilityAdvertisement(t *testing.T) {
	wire := "" +
		"000eversion 2\n" +
		"0013ls-refs=unborn\n" +
		"0012fetch=shallow\n" +
		"0013agent=git/test\n" +
		"0000"

	adv, err := decodeV2CapabilityAdvertisement(bytes.NewBufferString(wire))
	if err != nil {
		t.Fatalf("decode advertisement: %v", err)
	}
	if !adv.Supports("ls-refs") {
		t.Fatalf("expected ls-refs capability")
	}
	if got := adv.Value("fetch"); got != "shallow" {
		t.Fatalf("unexpected fetch value %q", got)
	}
	if got := adv.Value("agent"); got != "git/test" {
		t.Fatalf("unexpected agent value %q", got)
	}
}

func TestEncodeV2CommandRequest(t *testing.T) {
	req, err := encodeV2CommandRequest(
		"ls-refs",
		[]string{"agent=git-sync/test"},
		[]string{"peel", "ref-prefix refs/heads/"},
	)
	if err != nil {
		t.Fatalf("encode request: %v", err)
	}

	want := "" +
		"0014command=ls-refs\n" +
		"0018agent=git-sync/test\n" +
		"0001" +
		"0009peel\n" +
		"001bref-prefix refs/heads/\n" +
		"0000"
	if string(req) != want {
		t.Fatalf("unexpected request:\n%s\nwant:\n%s", req, want)
	}
}

func TestSourceFetchRequestV2IncludesIncludeTagForTagSync(t *testing.T) {
	adv := &v2CapabilityAdvertisement{
		Capabilities: map[string]string{
			"fetch": "thin-pack filter",
		},
	}
	desired := map[plumbing.ReferenceName]desiredRef{
		plumbing.NewTagReferenceName("v1"): {
			Kind:       RefKindTag,
			Label:      "v1",
			SourceRef:  plumbing.NewTagReferenceName("v1"),
			TargetRef:  plumbing.NewTagReferenceName("v1"),
			SourceHash: plumbing.NewHash("1111111111111111111111111111111111111111"),
		},
	}

	body, wants, haves, err := sourceFetchRequestV2(adv, desired, nil)
	if err != nil {
		t.Fatalf("build fetch request: %v", err)
	}
	if wants != 1 || haves != 0 {
		t.Fatalf("unexpected wants/haves: wants=%d haves=%d", wants, haves)
	}
	if !bytes.Contains(body, []byte("include-tag\n")) {
		t.Fatalf("expected include-tag in fetch request, got:\n%s", body)
	}
}
