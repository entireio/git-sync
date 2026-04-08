package syncer

import (
	"bytes"
	"testing"
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
