package gitproto

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

func BenchmarkPacketReaderData(b *testing.B) {
	// Build a wire-format buffer containing 1000 data packets.
	// Each packet is "0009data\n" (4-byte length + "data\n" = 9 bytes total).
	var wire strings.Builder
	const packetCount = 1000
	payload := "data\n"
	pkt := FormatPktLine(payload)
	for range packetCount {
		wire.WriteString(pkt)
	}
	wire.WriteString("0000") // flush to terminate
	data := wire.String()

	b.ResetTimer()
	for range b.N {
		reader := NewPacketReader(bytes.NewBufferString(data))
		for range packetCount {
			kind, p, err := reader.ReadPacket()
			if err != nil {
				b.Fatal(err)
			}
			if kind != PacketData || len(p) == 0 {
				b.Fatalf("unexpected packet: kind=%v len=%d", kind, len(p))
			}
		}
		// Read the trailing flush.
		kind, _, err := reader.ReadPacket()
		if err != nil {
			b.Fatal(err)
		}
		if kind != PacketFlush {
			b.Fatalf("expected flush, got %v", kind)
		}
	}
}

func BenchmarkDecodeV2Capabilities(b *testing.B) {
	// Build a capability advertisement with 10 capabilities.
	var wire strings.Builder
	wire.WriteString(FormatPktLine("version 2\n"))
	for i := range 10 {
		line := fmt.Sprintf("capability-%d=value-%d\n", i, i)
		wire.WriteString(FormatPktLine(line))
	}
	wire.WriteString("0000") // flush
	data := wire.String()

	b.ResetTimer()
	for range b.N {
		caps, err := DecodeV2Capabilities(bytes.NewBufferString(data))
		if err != nil {
			b.Fatal(err)
		}
		if len(caps.Caps) != 10 {
			b.Fatalf("expected 10 capabilities, got %d", len(caps.Caps))
		}
	}
}

func BenchmarkEncodeCommand(b *testing.B) {
	// Build a fetch command with 50 wants.
	capArgs := []string{"agent=gitsync/bench"}
	cmdArgs := make([]string, 0, 52)
	cmdArgs = append(cmdArgs, "ofs-delta", "no-progress")
	for i := range 50 {
		cmdArgs = append(cmdArgs, fmt.Sprintf("want %040x", i+1))
	}

	b.ResetTimer()
	for range b.N {
		data, err := EncodeCommand("fetch", capArgs, cmdArgs)
		if err != nil {
			b.Fatal(err)
		}
		if len(data) == 0 {
			b.Fatal("empty encoded command")
		}
	}
}
