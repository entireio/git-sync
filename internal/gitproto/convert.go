package gitproto

import (
	"fmt"
	"io"

	"github.com/go-git/go-git/v5/plumbing"
)

// PlannerDesired is the subset of planner.DesiredRef fields needed by gitproto.
// This avoids a circular import between gitproto and planner.
type PlannerDesired struct {
	SourceRef  plumbing.ReferenceName
	TargetRef  plumbing.ReferenceName
	SourceHash plumbing.Hash
	IsTag      bool
}

// ToPushCommands converts a slice of plan-like structs to PushCommands.
// Used by all strategy packages to avoid copy-pasting the conversion.
func ToPushCommands(plans []PushPlan) []PushCommand {
	cmds := make([]PushCommand, 0, len(plans))
	for _, p := range plans {
		cmd := PushCommand{Name: p.TargetRef, Old: p.TargetHash}
		if p.Delete {
			cmd.Delete = true
		} else {
			cmd.New = p.SourceHash
		}
		cmds = append(cmds, cmd)
	}
	return cmds
}

// PushPlan is a minimal interface for plan-to-command conversion.
type PushPlan struct {
	TargetRef  plumbing.ReferenceName
	TargetHash plumbing.Hash
	SourceHash plumbing.Hash
	Delete     bool
}

// LimitPackReader wraps a ReadCloser with a byte limit. Shared across strategies.
func LimitPackReader(r io.ReadCloser, maxBytes int64) io.ReadCloser {
	if maxBytes <= 0 {
		return r
	}
	return &packLimitRC{ReadCloser: r, max: maxBytes}
}

type packLimitRC struct {
	io.ReadCloser
	max  int64
	read int64
}

func (r *packLimitRC) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	r.read += int64(n)
	if r.read > r.max {
		return n, fmt.Errorf("source pack exceeded max-pack-bytes limit (%d)", r.max)
	}
	return n, err
}
