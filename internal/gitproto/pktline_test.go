package gitproto

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"testing"
)

func TestPacketReaderHandlesSpecialPackets(t *testing.T) {
	reader := NewPacketReader(bytes.NewBufferString("0000000100020006a\n"))

	kind, payload, err := reader.ReadPacket()
	if err != nil {
		t.Fatalf("read flush: %v", err)
	}
	if kind != PacketFlush || payload != nil {
		t.Fatalf("unexpected flush: kind=%v payload=%q", kind, payload)
	}

	kind, _, err = reader.ReadPacket()
	if err != nil {
		t.Fatalf("read delim: %v", err)
	}
	if kind != PacketDelim {
		t.Fatalf("unexpected delim kind: %v", kind)
	}

	kind, _, err = reader.ReadPacket()
	if err != nil {
		t.Fatalf("read response-end: %v", err)
	}
	if kind != PacketResponseEnd {
		t.Fatalf("unexpected response-end kind: %v", kind)
	}

	kind, payload, err = reader.ReadPacket()
	if err != nil {
		t.Fatalf("read data: %v", err)
	}
	if kind != PacketData || string(payload) != "a\n" {
		t.Fatalf("unexpected data: kind=%v payload=%q", kind, payload)
	}
}

func TestPacketReaderHandlesEmptyDataPacket(t *testing.T) {
	reader := NewPacketReader(bytes.NewBufferString("0004"))

	kind, payload, err := reader.ReadPacket()
	if err != nil {
		t.Fatalf("read empty data packet: %v", err)
	}
	if kind != PacketData {
		t.Fatalf("kind = %v, want PacketData", kind)
	}
	if len(payload) != 0 {
		t.Fatalf("payload length = %d, want 0", len(payload))
	}
}

func TestDecodeV2Capabilities(t *testing.T) {
	wire := "" +
		"000eversion 2\n" +
		"0013ls-refs=unborn\n" +
		"0012fetch=shallow\n" +
		"0013agent=git/test\n" +
		"0000"
	caps, err := DecodeV2Capabilities(bytes.NewBufferString(wire))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !caps.Supports("ls-refs") {
		t.Fatalf("expected ls-refs capability")
	}
	if got := caps.Value("fetch"); got != "shallow" {
		t.Fatalf("unexpected fetch value %q", got)
	}
	if got := caps.Value("agent"); got != "git/test" {
		t.Fatalf("unexpected agent value %q", got)
	}
}

func TestPacketReaderMalformedLength(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{
			name:  "non-hex characters",
			input: "xxxx",
		},
		{
			name:  "partial hex with invalid char",
			input: "00gz",
		},
		{
			name:  "uppercase non-hex",
			input: "ZZZZ",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reader := NewPacketReader(bytes.NewBufferString(tt.input))
			_, _, err := reader.ReadPacket()
			if err == nil {
				t.Fatal("expected error for malformed hex length, got nil")
			}
		})
	}
}

func TestPacketReaderTruncatedPayload(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{
			name:  "header claims 10 bytes total but payload is short",
			input: "000aab",
		},
		{
			name:  "header claims 8 bytes total but only 1 byte of payload",
			input: "0008x",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reader := NewPacketReader(bytes.NewBufferString(tt.input))
			_, _, err := reader.ReadPacket()
			if err == nil {
				t.Fatal("expected error for truncated payload, got nil")
			}
		})
	}
}

func TestDecodeV2CapabilitiesMissingVersion(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{
			name:  "wrong version string",
			input: "000eversion 1\n" + "0000",
		},
		{
			name:  "no version line at all, just capabilities",
			input: "0013ls-refs=unborn\n" + "0000",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := DecodeV2Capabilities(bytes.NewBufferString(tt.input))
			if err == nil {
				t.Fatal("expected error for missing version 2 line, got nil")
			}
		})
	}
}

func TestDecodeV2CapabilitiesEmptyFlush(t *testing.T) {
	// Flush packet (0000) before the version line should be skipped,
	// but if the stream is only flushes with no version line, it should error.
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{
			name:    "flush before version line is skipped",
			input:   "0000" + "000eversion 2\n" + "0013ls-refs=unborn\n" + "0000",
			wantErr: false,
		},
		{
			name:    "only flush packets with no data causes EOF",
			input:   "0000",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			caps, err := DecodeV2Capabilities(bytes.NewBufferString(tt.input))
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !caps.Supports("ls-refs") {
				t.Fatal("expected ls-refs capability after flush-then-version")
			}
		})
	}
}

func TestEncodeCommand(t *testing.T) {
	req, err := EncodeCommand(
		"ls-refs",
		[]string{"agent=git-sync/test"},
		[]string{"peel", "ref-prefix refs/heads/"},
	)
	if err != nil {
		t.Fatalf("encode: %v", err)
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

func TestEncodeCommandNoArgs(t *testing.T) {
	// EncodeCommand with no command args should emit command + cap args +
	// flush, with no delimiter section.
	req, err := EncodeCommand(
		"ls-refs",
		[]string{"agent=git-sync/test"},
		nil,
	)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	want := "" +
		"0014command=ls-refs\n" +
		"0018agent=git-sync/test\n" +
		"0000"
	if string(req) != want {
		t.Fatalf("unexpected request:\ngot:  %q\nwant: %q", string(req), want)
	}
	// Verify no delimiter is present in the output.
	if bytes.Contains(req, []byte("0001")) {
		t.Fatalf("expected no delimiter section when cmdArgs is nil, but found 0001")
	}
}

func TestPacketReaderEOF(t *testing.T) {
	// Reading from an empty reader should return io.EOF.
	reader := NewPacketReader(bytes.NewReader(nil))
	_, _, err := reader.ReadPacket()
	if err == nil {
		t.Fatal("expected error from empty reader, got nil")
	}
	if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("expected io.EOF or io.ErrUnexpectedEOF, got %v", err)
	}
}

func TestSkipSection(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantErr   bool
		remainder string
	}{
		{
			name: "data packets followed by delimiter",
			// Two data packets then a delimiter (0001), then a trailing data packet.
			input:     "0009hello" + "0009world" + "0001" + "0008end!",
			remainder: "0008end!",
		},
		{
			name: "data packets followed by flush",
			// Two data packets then a flush (0000).
			input: "0009hello" + "0009world" + "0000",
		},
		{
			name:    "EOF before delimiter or flush",
			input:   "0009hello",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			buf := bytes.NewBufferString(tt.input)
			pr := NewPacketReader(buf)
			err := SkipSection(pr)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			// Verify that the remainder after the section is intact.
			if tt.remainder != "" {
				kind, payload, err := pr.ReadPacket()
				if err != nil {
					t.Fatalf("reading remainder: %v", err)
				}
				if kind != PacketData {
					t.Fatalf("expected data packet after skip, got kind=%v", kind)
				}
				if string(payload) != "end!" {
					t.Fatalf("remainder payload = %q, want %q", payload, "end!")
				}
			}
		})
	}
}

func TestFormatPktLine(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{input: "a\n", want: "0006a\n"},
		{input: "hello\n", want: "000ahello\n"},
		{input: "", want: "0004"},
		{input: "version 2\n", want: "000eversion 2\n"},
	}
	for _, tt := range tests {
		got := FormatPktLine(tt.input)
		if got != tt.want {
			t.Errorf("FormatPktLine(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestNewPacketReaderWithBufioReader(t *testing.T) {
	// When passed a *bufio.Reader, NewPacketReader should reuse it instead
	// of wrapping it again.
	input := bytes.NewBufferString("0000")
	br := bufio.NewReader(input)
	pr := NewPacketReader(br)
	if pr.BufReader() != br {
		t.Error("expected NewPacketReader to reuse the provided *bufio.Reader")
	}
	kind, _, err := pr.ReadPacket()
	if err != nil {
		t.Fatalf("ReadPacket error: %v", err)
	}
	if kind != PacketFlush {
		t.Errorf("expected PacketFlush, got %v", kind)
	}
}

func TestParseHexLengthUppercase(t *testing.T) {
	// Uppercase hex should also parse correctly (hexVal covers A-F).
	// 0x000A = 10 total, payload = 10 - 4 = 6 bytes.
	wire := "000AABCDEF"
	pr := NewPacketReader(bytes.NewBufferString(wire))
	kind, payload, err := pr.ReadPacket()
	if err != nil {
		t.Fatalf("ReadPacket error: %v", err)
	}
	if kind != PacketData {
		t.Fatalf("expected PacketData, got %v", kind)
	}
	if string(payload) != "ABCDEF" {
		t.Errorf("payload = %q, want %q", payload, "ABCDEF")
	}
}

func TestHashHex(t *testing.T) {
	tests := []struct {
		name string
		hash [20]byte
		want string
	}{
		{
			name: "zero hash",
			hash: [20]byte{},
			want: "0000000000000000000000000000000000000000",
		},
		{
			name: "known hash",
			hash: [20]byte{0xde, 0xad, 0xbe, 0xef, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10},
			want: "deadbeef0102030405060708090a0b0c0d0e0f10",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := HashHex(tt.hash)
			if got != tt.want {
				t.Errorf("HashHex() = %q, want %q", got, tt.want)
			}
		})
	}
}
