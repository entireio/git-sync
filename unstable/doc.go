// Package unstable exposes advanced git-sync controls and commands that are
// intentionally outside the stable gitsync surface.
//
// This package exists for first-party consumers such as the CLI and benchmark
// tool that still need direct access to engine-adjacent controls like batch
// sizing, heap measurement, verbose progress, and fetch/bootstrap entrypoints.
//
// The API in this package is explicitly not stable.
package unstable
