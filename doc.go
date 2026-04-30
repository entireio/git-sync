// Package gitsync provides the public orchestration API for gitsync.
//
// The public surface is intentionally narrower than the internal engine:
// callers express sync intent through typed probe, plan, and sync requests,
// while relay selection, batching, and fallback strategy remain internal.
//
// The package is designed for embedders such as queue workers. Callers can:
//   - inject an HTTP client for transport, OTEL, proxy, TLS, and timeout control
//   - inject an auth provider that resolves source and target credentials
//   - inspect structured results for per-ref outcomes and aggregate counters
//
// Current advanced engine tuning such as batch sizing, max pack thresholds, and
// heap measurement remains outside this stable public surface.
package gitsync
