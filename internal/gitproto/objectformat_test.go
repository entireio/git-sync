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

// A source advertising a format git-sync cannot decode fails at ref listing,
// before any fetch, instead of with a checksum error mid-pack.
func TestListSourceRefsRejectsUnsupportedObjectFormat(t *testing.T) {
	t.Parallel()
	conn := &stubConn{
		reqInfoRefs: func(_ context.Context, _ string, gitProtocol string) ([]byte, error) {
			if gitProtocol == GitProtocolV2 {
				return nil, errors.New("v2 not offered")
			}
			var body strings.Builder
			mustWrite := func(err error) {
				if err != nil {
					t.Fatalf("write advertisement: %v", err)
				}
			}
			_, err := pktline.Writef(&body, "# service=%s\n", transport.UploadPackService)
			mustWrite(err)
			mustWrite(pktline.WriteFlush(&body))
			_, err = pktline.Writef(&body, "%s refs/heads/main\x00%s=sha512\n", strings.Repeat("a", 40), capability.ObjectFormat)
			mustWrite(err)
			mustWrite(pktline.WriteFlush(&body))
			return []byte(body.String()), nil
		},
	}

	_, _, err := ListSourceRefs(t.Context(), conn, "auto", nil)
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
