package gitproto

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"fmt"
	"io"

	"github.com/go-git/go-git/v6/plumbing/format/pktline"
)

// PacketType represents the type of a pkt-line packet.
type PacketType int

const (
	PacketData        PacketType = iota
	PacketFlush                  // 0000
	PacketDelim                  // 0001
	PacketResponseEnd            // 0002
)

// PacketReader reads pkt-line formatted data. It reuses a fixed header buffer
// and a growable payload buffer to reduce allocations (issue #17).
type PacketReader struct {
	r      *bufio.Reader
	header [4]byte
	buf    []byte
}

func NewPacketReader(r io.Reader) *PacketReader {
	if br, ok := r.(*bufio.Reader); ok {
		return &PacketReader{r: br, buf: make([]byte, 0, 1024)}
	}
	return &PacketReader{r: bufio.NewReaderSize(r, 65536), buf: make([]byte, 0, 1024)}
}

// BufReader returns the underlying buffered reader for direct access
// (needed by sideband demuxer after switching to pack stream).
func (pr *PacketReader) BufReader() *bufio.Reader {
	return pr.r
}

// ReadPacket reads the next pkt-line packet. The returned payload slice is
// only valid until the next call to ReadPacket.
func (pr *PacketReader) ReadPacket() (PacketType, []byte, error) {
	if _, err := io.ReadFull(pr.r, pr.header[:]); err != nil {
		return PacketData, nil, err
	}

	switch string(pr.header[:]) {
	case "0000":
		return PacketFlush, nil, nil
	case "0001":
		return PacketDelim, nil, nil
	case "0002":
		return PacketResponseEnd, nil, nil
	}

	n, err := parseHexLength(pr.header)
	if err != nil {
		return PacketData, nil, err
	}
	if n <= 4 {
		return PacketData, nil, pktline.ErrInvalidPktLen
	}

	payloadLen := n - 4
	if payloadLen > cap(pr.buf) {
		pr.buf = make([]byte, payloadLen)
	} else {
		pr.buf = pr.buf[:payloadLen]
	}
	if _, err := io.ReadFull(pr.r, pr.buf); err != nil {
		return PacketData, nil, err
	}
	return PacketData, pr.buf, nil
}

func parseHexLength(header [4]byte) (int, error) {
	var n int
	for _, b := range header {
		v, ok := hexVal(b)
		if !ok {
			return 0, pktline.ErrInvalidPktLen
		}
		n = 16*n + int(v)
	}
	return n, nil
}

func hexVal(b byte) (byte, bool) {
	switch {
	case b >= '0' && b <= '9':
		return b - '0', true
	case b >= 'a' && b <= 'f':
		return b - 'a' + 10, true
	case b >= 'A' && b <= 'F':
		return b - 'A' + 10, true
	default:
		return 0, false
	}
}

// EncodeCommand builds a pkt-line encoded v2 command request.
func EncodeCommand(command string, capArgs, cmdArgs []string) ([]byte, error) {
	var buf bytes.Buffer
	if _, err := pktline.Writef(&buf, "command=%s\n", command); err != nil {
		return nil, err
	}
	for _, arg := range capArgs {
		if _, err := pktline.Writef(&buf, "%s\n", arg); err != nil {
			return nil, err
		}
	}
	if len(cmdArgs) > 0 {
		if err := pktline.WriteDelim(&buf); err != nil {
			return nil, err
		}
		for _, arg := range cmdArgs {
			if _, err := pktline.Writef(&buf, "%s\n", arg); err != nil {
				return nil, err
			}
		}
	}
	if err := pktline.WriteFlush(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// SkipSection reads and discards packets until a delimiter or flush is reached.
func SkipSection(pr *PacketReader) error {
	for {
		kind, _, err := pr.ReadPacket()
		if err != nil {
			return err
		}
		if kind == PacketDelim || kind == PacketFlush {
			return nil
		}
	}
}

// HashHex is a helper to encode a 20-byte hash as lowercase hex.
func HashHex(h [20]byte) string {
	return hex.EncodeToString(h[:])
}

// FormatPktLine encodes a single pkt-line from a string payload.
func FormatPktLine(s string) string {
	n := len(s) + 4
	return fmt.Sprintf("%04x%s", n, s)
}
