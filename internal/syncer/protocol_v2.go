package syncer

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"strings"

	"github.com/go-git/go-git/v5/plumbing/format/pktline"
)

const (
	delimPkt       = "0001"
	responseEndPkt = "0002"
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

	kind, payload, err := reader.ReadPacket()
	if err != nil {
		return nil, err
	}
	if kind != packetTypeData || string(payload) != "version 2\n" {
		return nil, fmt.Errorf("unexpected protocol advertisement %q", payload)
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
