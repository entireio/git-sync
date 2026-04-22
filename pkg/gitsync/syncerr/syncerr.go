// Package syncerr defines error types returned by git-sync's public API.
// It lives in its own subpackage so low-level gitproto code can construct
// these types without importing pkg/gitsync (which would create a cycle
// through internalbridge).
package syncerr

import (
	"fmt"
	"strings"
)

// PushRefFailure is a single per-reference failure from a receive-pack
// report-status response.
type PushRefFailure struct {
	// Ref is the reference name the target rejected, e.g. "refs/heads/main".
	Ref string
	// Status is the server-provided reason. Values are server-specific —
	// entiredb emits "remote ref has changed" for CAS failures and
	// "already exists" for create-on-nonempty races; other servers may
	// emit different strings.
	Status string
}

// PushReportError is returned by the public Sync / Replicate paths when
// receive-pack's report-status contained either an unpack-phase failure
// or one or more per-reference command failures. Callers inspecting
// Failures can decide whether to reconcile (e.g. verify the target is
// already at the desired state) or surface the failure.
type PushReportError struct {
	// UnpackStatus is set when the unpack phase itself failed. When set,
	// Failures is empty and no per-reference decisions were made.
	UnpackStatus string
	// Failures holds every per-reference command that did not return "ok".
	Failures []PushRefFailure
}

func (e *PushReportError) Error() string {
	if e.UnpackStatus != "" {
		return "report-status: unpack error: " + e.UnpackStatus
	}
	if len(e.Failures) == 0 {
		return "report-status: unknown failure"
	}
	var b strings.Builder
	b.WriteString("report-status: command error on ")
	for i, f := range e.Failures {
		if i > 0 {
			b.WriteString("; ")
		}
		fmt.Fprintf(&b, "%s: %s", f.Ref, f.Status)
	}
	return b.String()
}
