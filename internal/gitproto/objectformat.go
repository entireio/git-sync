package gitproto

import (
	"errors"
	"fmt"

	formatcfg "github.com/go-git/go-git/v6/plumbing/format/config"
	"github.com/go-git/go-git/v6/plumbing/protocol/capability"
	"github.com/go-git/go-git/v6/plumbing/protocol/packp"
)

// ErrUnsupportedObjectFormat is reported (wrapped) when a remote advertises an
// object-format other than sha1 or sha256. go-git sizes ids and pack trailers
// for an unrecognised format as sha1, so going on would fail later with a
// checksum or parse error that hides the cause.
var ErrUnsupportedObjectFormat = errors.New("unsupported object format")

// ErrObjectFormatMismatch is reported (wrapped) when the source and target
// repositories use different object formats. git-sync relays the source's
// object ids and packs unchanged and cannot convert between formats, so the
// target must be created in the source's format.
var ErrObjectFormatMismatch = errors.New("source and target object formats differ")

// ValidateObjectFormat returns an error wrapping ErrUnsupportedObjectFormat
// unless f is one git-sync handles: unset (a remote that advertises none, which
// the protocol defines as sha1), sha1 or sha256.
func ValidateObjectFormat(f formatcfg.ObjectFormat) error {
	switch f {
	case formatcfg.UnsetObjectFormat, formatcfg.SHA1, formatcfg.SHA256:
		return nil
	default:
		return fmt.Errorf("%w %q", ErrUnsupportedObjectFormat, string(f))
	}
}

// CheckObjectFormats validates both formats and returns an error wrapping
// ErrObjectFormatMismatch when they differ. A remote that advertises no format
// is sha1: servers before git 2.28 never advertise one, while a sha256
// repository always does.
func CheckObjectFormats(source, target formatcfg.ObjectFormat) error {
	if err := ValidateObjectFormat(source); err != nil {
		return fmt.Errorf("source: %w", err)
	}
	if err := ValidateObjectFormat(target); err != nil {
		return fmt.Errorf("target: %w", err)
	}
	src, dst := effectiveObjectFormat(source), effectiveObjectFormat(target)
	if src != dst {
		return fmt.Errorf("%w: source is %s, target is %s; git-sync cannot convert between object formats, so the target must be a %s repository",
			ErrObjectFormatMismatch, src, dst, src)
	}
	return nil
}

// AdvRefsObjectFormat returns the object-format a v0/v1 advertisement
// declares, or UnsetObjectFormat when it declares none.
func AdvRefsObjectFormat(adv *packp.AdvRefs) formatcfg.ObjectFormat {
	if adv == nil {
		return formatcfg.UnsetObjectFormat
	}
	if vals := adv.Capabilities.Get(capability.ObjectFormat); len(vals) > 0 {
		return formatcfg.ObjectFormat(vals[0])
	}
	return formatcfg.UnsetObjectFormat
}

// effectiveObjectFormat maps a format that was not advertised to sha1, the
// protocol's default.
func effectiveObjectFormat(f formatcfg.ObjectFormat) formatcfg.ObjectFormat {
	if f == formatcfg.UnsetObjectFormat {
		return formatcfg.SHA1
	}
	return f
}
