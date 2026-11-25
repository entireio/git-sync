package gitproto

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/go-git/go-git/v6/plumbing"
	formatcfg "github.com/go-git/go-git/v6/plumbing/format/config"
	"github.com/go-git/go-git/v6/plumbing/format/pktline"
	"github.com/go-git/go-git/v6/plumbing/protocol/capability"
	"github.com/go-git/go-git/v6/plumbing/protocol/packp"
	"github.com/go-git/go-git/v6/plumbing/transport"
)

func TestValidateObjectFormat(t *testing.T) {
	t.Parallel()
	for _, f := range []formatcfg.ObjectFormat{formatcfg.UnsetObjectFormat, formatcfg.SHA1, formatcfg.SHA256} {
		if err := ValidateObjectFormat(f); err != nil {
			t.Errorf("ValidateObjectFormat(%q) = %v, want nil", f, err)
		}
	}
	// go-git sizes an unrecognised format as sha1, so it must not get through.
	err := ValidateObjectFormat("sha512")
	if !errors.Is(err, ErrUnsupportedObjectFormat) {
		t.Fatalf("ValidateObjectFormat(sha512) = %v, want ErrUnsupportedObjectFormat", err)
	}
	if !strings.Contains(err.Error(), `"sha512"`) {
		t.Errorf("error %q does not name the format", err)
	}
}

func TestCheckObjectFormats(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name           string
		source, target formatcfg.ObjectFormat
		want           error
		wantText       string
	}{
		{name: "sha1 both", source: formatcfg.SHA1, target: formatcfg.SHA1},
		{name: "sha256 both", source: formatcfg.SHA256, target: formatcfg.SHA256},
		// A server before git 2.28 advertises no format and is sha1.
		{name: "unadvertised source is sha1", source: formatcfg.UnsetObjectFormat, target: formatcfg.SHA1},
		{name: "unadvertised target is sha1", source: formatcfg.SHA1, target: formatcfg.UnsetObjectFormat},
		{name: "neither advertised", source: formatcfg.UnsetObjectFormat, target: formatcfg.UnsetObjectFormat},
		{name: "sha1 to sha256", source: formatcfg.SHA1, target: formatcfg.SHA256, want: ErrObjectFormatMismatch, wantText: "source is sha1, target is sha256"},
		{name: "sha256 to sha1", source: formatcfg.SHA256, target: formatcfg.SHA1, want: ErrObjectFormatMismatch, wantText: "source is sha256, target is sha1"},
		{name: "sha256 to unadvertised", source: formatcfg.SHA256, target: formatcfg.UnsetObjectFormat, want: ErrObjectFormatMismatch, wantText: "target is sha1"},
		{name: "unknown source", source: "sha512", target: formatcfg.SHA256, want: ErrUnsupportedObjectFormat, wantText: "source: "},
		{name: "unknown target", source: formatcfg.SHA256, target: "sha512", want: ErrUnsupportedObjectFormat, wantText: "target: "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := CheckObjectFormats(tc.source, tc.target)
			if tc.want == nil {
				if err != nil {
					t.Fatalf("CheckObjectFormats = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("CheckObjectFormats = %v, want %v", err, tc.want)
			}
			if !strings.Contains(err.Error(), tc.wantText) {
				t.Errorf("error %q does not contain %q", err, tc.wantText)
			}
		})
	}
}

// advLayout varies the parts of a v0/v1 advertisement that sit around the
// first ref line without changing its meaning.
type advLayout struct {
	name string
	// versionLine announces "version 1" before the refs, as a server does
	// for a client that asked for protocol v1.
	versionLine bool
	// capPad lengthens the first ref line's capability list by this many
	// bytes, past any small read buffer.
	capPad int
	// noBanner leaves out the smart-HTTP "# service=" announcement, as SSH
	// and remote-helper transports do, so the first packet is a ref.
	noBanner bool
}

var advLayouts = []advLayout{
	{name: "plain"},
	{name: "version 1 first", versionLine: true},
	{name: "first line over 4 KiB", capPad: 5000},
	{name: "first line near the pkt-line limit", capPad: 65000},
	{name: "version 1 then long first line", versionLine: true, capPad: 5000},
	{name: "no banner", noBanner: true},
	{name: "no banner, first line over 4 KiB", noBanner: true, capPad: 5000},
	{name: "no banner, version 1 then long first line", noBanner: true, versionLine: true, capPad: 5000},
}

// v1AdvertisementWithFormat is a smart-HTTP v0/v1 advertisement whose ids are
// hexLen characters wide and whose capabilities declare format, the way a
// server of that format writes it.
func v1AdvertisementWithFormat(t *testing.T, service, format string, hexLen int, layout advLayout) []byte {
	t.Helper()
	var body strings.Builder
	mustWrite := func(_ int, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("write advertisement: %v", err)
		}
	}
	if !layout.noBanner {
		mustWrite(pktline.Writef(&body, "# service=%s\n", service))
		mustWrite(0, pktline.WriteFlush(&body))
	}
	if layout.versionLine {
		mustWrite(pktline.WriteString(&body, "version 1\n"))
	}
	id := strings.Repeat("a", hexLen)
	agent := "agent=git/test" + strings.Repeat("x", layout.capPad)
	mustWrite(pktline.Writef(&body, "%s refs/heads/main\x00%s=%s report-status %s\n", id, capability.ObjectFormat, format, agent))
	mustWrite(pktline.Writef(&body, "%s refs/heads/other\n", id))
	mustWrite(0, pktline.WriteFlush(&body))
	return []byte(body.String())
}

// A v1 remote of an unsupported format writes ids of that format's width,
// which go-git's ref decoder rejects as "invalid size" before anything reads
// the capability. The format must be found in the first ref line first, on
// both the source (upload-pack) and the target (receive-pack), wherever that
// line sits and however long it is.
func TestV1AdvertisementRejectsUnsupportedObjectFormat(t *testing.T) {
	t.Parallel()
	const sha512HexLen = 128

	for _, layout := range advLayouts {
		t.Run(layout.name, func(t *testing.T) {
			t.Parallel()
			t.Run("source", func(t *testing.T) {
				t.Parallel()
				conn := &stubConn{
					reqInfoRefs: func(_ context.Context, service string, gitProtocol string) ([]byte, error) {
						if gitProtocol == GitProtocolV2 {
							return nil, errors.New("v2 not offered")
						}
						return v1AdvertisementWithFormat(t, service, "sha512", sha512HexLen, layout), nil
					},
				}
				_, _, err := ListSourceRefs(t.Context(), conn, "v1", nil)
				if !errors.Is(err, ErrUnsupportedObjectFormat) {
					t.Fatalf("ListSourceRefs = %v, want ErrUnsupportedObjectFormat", err)
				}
			})
			t.Run("target", func(t *testing.T) {
				t.Parallel()
				conn := &stubConn{
					reqInfoRefs: func(_ context.Context, service string, _ string) ([]byte, error) {
						return v1AdvertisementWithFormat(t, service, "sha512", sha512HexLen, layout), nil
					},
				}
				_, err := AdvertisedRefsV1(t.Context(), conn, transport.ReceivePackService)
				if !errors.Is(err, ErrUnsupportedObjectFormat) {
					t.Fatalf("AdvertisedRefsV1 = %v, want ErrUnsupportedObjectFormat", err)
				}
			})
			// The check must not get in the way of the formats that decode.
			for _, tc := range []struct {
				format string
				hexLen int
			}{{"sha1", 40}, {"sha256", 64}} {
				t.Run("accepts "+tc.format, func(t *testing.T) {
					t.Parallel()
					conn := &stubConn{
						reqInfoRefs: func(_ context.Context, service string, _ string) ([]byte, error) {
							return v1AdvertisementWithFormat(t, service, tc.format, tc.hexLen, layout), nil
						},
					}
					adv, err := AdvertisedRefsV1(t.Context(), conn, transport.ReceivePackService)
					if err != nil {
						t.Fatalf("AdvertisedRefsV1: %v", err)
					}
					if got := AdvRefsObjectFormat(adv); string(got) != tc.format {
						t.Errorf("object format = %q, want %q", got, tc.format)
					}
				})
			}
		})
	}
}

// A v2 source's format is known from its capability advertisement; reject an
// unsupported one there, before ls-refs reads ids at the wrong width.
func TestListSourceRefsV2RejectsUnsupportedObjectFormatBeforeLsRefs(t *testing.T) {
	t.Parallel()
	conn := &stubConn{
		reqInfoRefs: func(_ context.Context, _ string, gitProtocol string) ([]byte, error) {
			if gitProtocol != GitProtocolV2 {
				return nil, errors.New("only v2 offered")
			}
			var body strings.Builder
			for _, line := range []string{"version 2\n", "ls-refs=unborn\n", "fetch=shallow\n", "object-format=sha512\n"} {
				if _, err := pktline.WriteString(&body, line); err != nil {
					t.Fatalf("write capabilities: %v", err)
				}
			}
			if err := pktline.WriteFlush(&body); err != nil {
				t.Fatalf("write flush: %v", err)
			}
			return []byte(body.String()), nil
		},
	}
	// stubConn fails any POST, so reaching ls-refs would surface a different error.
	_, _, err := ListSourceRefs(t.Context(), conn, "v2", nil)
	if !errors.Is(err, ErrUnsupportedObjectFormat) {
		t.Fatalf("ListSourceRefs = %v, want ErrUnsupportedObjectFormat", err)
	}
}

// A v1 client that omits object-format is assumed to be sha1; declare the
// format the source advertised, and none when it advertised none.
func TestBuildV1UploadPackBodyDeclaresObjectFormat(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		advertised string
		wantHex    int
	}{
		{name: "sha256", advertised: "sha256", wantHex: 64},
		{name: "sha1", advertised: "sha1", wantHex: 40},
		{name: "not advertised", wantHex: 40},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			adv := &packp.AdvRefs{}
			if tc.advertised != "" {
				adv.Capabilities.Set(capability.ObjectFormat, tc.advertised)
			}
			want := plumbing.NewHash(strings.Repeat("a", tc.wantHex))
			desired := map[plumbing.ReferenceName]DesiredRef{
				"refs/heads/main": {SourceRef: "refs/heads/main", TargetRef: "refs/heads/main", SourceHash: want},
			}
			_, caps, err := buildV1UploadPackBody(adv, desired, nil, false, false, false)
			if err != nil {
				t.Fatalf("buildV1UploadPackBody: %v", err)
			}
			got := caps.Get(capability.ObjectFormat)
			switch {
			case tc.advertised == "" && len(got) != 0:
				t.Errorf("object-format = %q, want none when the source advertised none", got)
			case tc.advertised != "" && (len(got) != 1 || got[0] != tc.advertised):
				t.Errorf("object-format = %q, want %q", got, tc.advertised)
			}
		})
	}
}
