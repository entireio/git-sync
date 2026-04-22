package gitsync

import "github.com/entirehq/git-sync/pkg/gitsync/syncerr"

// PushRefFailure and PushReportError are defined in the syncerr subpackage
// so internal gitproto can construct them without creating an import cycle
// through internalbridge. They are re-exported here as type aliases so the
// public API surface is cohesive and errors.As works with either name.
type (
	PushRefFailure  = syncerr.PushRefFailure
	PushReportError = syncerr.PushReportError
)
