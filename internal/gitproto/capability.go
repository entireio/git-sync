package gitproto

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/go-git/go-git/v6/plumbing/protocol/packp/capability"
)

// V2Capabilities represents a parsed protocol v2 capability advertisement.
type V2Capabilities struct {
	Caps map[string]string
}

// Supports reports whether the server advertises the named capability.
func (c *V2Capabilities) Supports(name string) bool {
	if c == nil {
		return false
	}
	_, ok := c.Caps[name]
	return ok
}

// Value returns the value string for the named capability.
func (c *V2Capabilities) Value(name string) string {
	if c == nil {
		return ""
	}
	return c.Caps[name]
}

// FetchSupports checks whether a specific feature is listed in the
// "fetch" capability value (space-separated feature list).
func (c *V2Capabilities) FetchSupports(feature string) bool {
	if c == nil {
		return false
	}
	for _, f := range strings.Fields(c.Value("fetch")) {
		if f == feature {
			return true
		}
	}
	return false
}

// SortedKeys returns the capabilities as sorted "key" or "key=value" strings.
func (c *V2Capabilities) SortedKeys() []string {
	if c == nil {
		return nil
	}
	keys := make([]string, 0, len(c.Caps))
	for k, v := range c.Caps {
		if v == "" {
			keys = append(keys, k)
		} else {
			keys = append(keys, k+"="+v)
		}
	}
	sort.Strings(keys)
	return keys
}

// RequestCapabilities builds the capability arguments for a v2 command request.
func (c *V2Capabilities) RequestCapabilities() []string {
	var caps []string
	if agent := c.Value("agent"); agent != "" {
		caps = append(caps, "agent="+capability.DefaultAgent())
	}
	return caps
}

// DecodeV2Capabilities parses a protocol v2 capability advertisement from an
// info/refs response body.
func DecodeV2Capabilities(r io.Reader) (*V2Capabilities, error) {
	reader := NewPacketReader(r)

	// Skip service line and find "version 2\n"
	for {
		kind, payload, err := reader.ReadPacket()
		if err != nil {
			return nil, err
		}
		if kind == PacketFlush {
			continue
		}
		if kind != PacketData {
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

	caps := &V2Capabilities{Caps: make(map[string]string)}
	for {
		kind, payload, err := reader.ReadPacket()
		if err != nil {
			return nil, err
		}
		if kind == PacketFlush {
			return caps, nil
		}
		if kind != PacketData {
			return nil, fmt.Errorf("unexpected packet type %v in capability advertisement", kind)
		}
		line := strings.TrimSuffix(string(payload), "\n")
		name, value, _ := strings.Cut(line, "=")
		caps.Caps[name] = value
	}
}

// PreferredSideband returns the best sideband capability supported by the
// server. Prefers sideband-64k over sideband (issue #4: sideband preference
// was backwards in original code).
func PreferredSideband(caps *capability.List) capability.Capability {
	if caps.Supports(capability.Sideband64k) {
		return capability.Sideband64k
	}
	if caps.Supports(capability.Sideband) {
		return capability.Sideband
	}
	return ""
}
