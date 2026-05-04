package syncer

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"entire.io/entire/git-sync/internal/gitproto"
)

// ServiceStats tracks transfer statistics for a single service.
type ServiceStats struct {
	Name          string `json:"name"`
	Requests      int    `json:"requests"`
	RequestBytes  int64  `json:"requestBytes"`
	ResponseBytes int64  `json:"responseBytes"`
	Wants         int    `json:"wants"`
	Haves         int    `json:"haves"`
	Commands      int    `json:"commands"`
}

// SideBytes reports cumulative bytes transferred at one transport label
// ("source" or "target"). Source is dominated by upload-pack response
// bytes (download), target is dominated by receive-pack request bytes
// (upload), so this is a useful "data moved on this side" number for both.
//
// Display carries the hostname extracted from the endpoint URL so live
// renderers can show "github.com → … → host" without re-parsing the URL.
// Empty when the endpoint URL was not http(s) or failed to parse.
//
// ActiveNanos and IdleNanos let renderers freeze the per-side rate once
// a transfer finishes: ActiveNanos spans from stats start to the most
// recent byte read, so dividing Bytes by ActiveNanos yields the rate
// during active streaming rather than a value that decays as wall clock
// keeps advancing past the last byte. IdleNanos is the gap between the
// last byte and the snapshot, used to mark a side as "done".
type SideBytes struct {
	Label       string `json:"label"`
	Bytes       int64  `json:"bytes"`
	Display     string `json:"display,omitempty"`
	ActiveNanos int64  `json:"activeNanos,omitempty"`
	IdleNanos   int64  `json:"idleNanos,omitempty"`
}

// Stats holds the collected transfer statistics.
type Stats struct {
	Enabled      bool                     `json:"enabled"`
	Items        map[string]*ServiceStats `json:"items"`
	Sides        []SideBytes              `json:"sides,omitempty"`
	ElapsedNanos int64                    `json:"elapsedNanos,omitempty"`
}

// sideCounter holds atomic byte counters for one transport label.
// Updated from the request and response body wrappers without holding
// the collector mutex, so a progress reader can sample at high
// frequency without contending with active transfers.
//
// lastByteAt is the unix-nanos timestamp of the most recent non-empty
// Read. It is used to freeze the displayed rate once a transfer
// finishes — without it, rate = bytes / (now − start) keeps shrinking
// as wall clock advances past the actual end of the transfer.
//
// display is set once during session setup (via setSideDisplay) and
// then read by the live progress reporter; it does not need a lock
// because writes happen-before any reader goroutine starts.
type sideCounter struct {
	bytes      atomic.Int64
	lastByteAt atomic.Int64
	display    string
}

// statsCollector is a concurrency-safe stats collector.
type statsCollector struct {
	enabled   bool
	startedAt time.Time
	mu        sync.Mutex
	items     map[string]*ServiceStats
	sidesMu   sync.RWMutex
	sides     map[string]*sideCounter
	// phase carries an optional one-line activity label
	// (e.g. "pack 3/8") that the live progress reporter renders next
	// to the per-side counters. Updated atomically by strategies and
	// read by the reporter goroutine without contention.
	phase atomic.Pointer[string]
}

func newStats(enabled bool) *statsCollector {
	return &statsCollector{
		enabled:   enabled,
		startedAt: time.Now(),
		items:     map[string]*ServiceStats{},
		sides:     map[string]*sideCounter{},
	}
}

func (s *statsCollector) ensure(name string) *ServiceStats {
	item, ok := s.items[name]
	if !ok {
		item = &ServiceStats{Name: name}
		s.items[name] = item
	}
	return item
}

func (s *statsCollector) recordRoundTrip(name string, requestBytes, responseBytes int64) {
	if !s.enabled {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	item := s.ensure(name)
	item.Requests++
	item.RequestBytes += requestBytes
	item.ResponseBytes += responseBytes
}

// side returns the per-label counter, creating it lazily on first use.
// Counters are tracked unconditionally (independent of ShowStats) so the
// live progress ticker works without forcing --stats.
func (s *statsCollector) side(label string) *sideCounter {
	s.sidesMu.RLock()
	if sc, ok := s.sides[label]; ok {
		s.sidesMu.RUnlock()
		return sc
	}
	s.sidesMu.RUnlock()

	s.sidesMu.Lock()
	defer s.sidesMu.Unlock()
	if sc, ok := s.sides[label]; ok {
		return sc
	}
	sc := &sideCounter{}
	s.sides[label] = sc
	return sc
}

// setPhase records a short activity label that the live progress
// reporter will surface alongside per-side counters. Pass "" to clear.
func (s *statsCollector) setPhase(p string) {
	s.phase.Store(&p)
}

// getPhase returns the most recent phase label, or "" if none was set.
func (s *statsCollector) getPhase() string {
	if p := s.phase.Load(); p != nil {
		return *p
	}
	return ""
}

// setSideDisplay attaches a human-readable name (typically the URL
// hostname) to a side. Called once during session setup so the live
// renderer can print "github.com → ..." instead of "source: ...".
func (s *statsCollector) setSideDisplay(label, display string) {
	if display == "" {
		return
	}
	sc := s.side(label)
	s.sidesMu.Lock()
	sc.display = display
	s.sidesMu.Unlock()
}

// liveSides returns a snapshot of per-side byte totals for live rendering.
// ActiveNanos and IdleNanos are computed against time.Now() at snapshot
// time so callers do not need to know the collector's start instant.
func (s *statsCollector) liveSides() []SideBytes {
	s.sidesMu.RLock()
	defer s.sidesMu.RUnlock()
	startNanos := s.startedAt.UnixNano()
	nowNanos := time.Now().UnixNano()
	out := make([]SideBytes, 0, len(s.sides))
	for label, sc := range s.sides {
		bytes := sc.bytes.Load()
		lastByte := sc.lastByteAt.Load()
		var activeNanos, idleNanos int64
		if lastByte > 0 {
			activeNanos = lastByte - startNanos
			if activeNanos < 0 {
				activeNanos = 0
			}
			idleNanos = nowNanos - lastByte
			if idleNanos < 0 {
				idleNanos = 0
			}
		}
		out = append(out, SideBytes{
			Label:       label,
			Bytes:       bytes,
			Display:     sc.display,
			ActiveNanos: activeNanos,
			IdleNanos:   idleNanos,
		})
	}
	return out
}

func (s *statsCollector) snapshot() Stats {
	s.mu.Lock()
	out := Stats{Enabled: s.enabled, Items: make(map[string]*ServiceStats, len(s.items))}
	for key, item := range s.items {
		copyItem := *item
		out.Items[key] = &copyItem
	}
	s.mu.Unlock()
	if s.enabled {
		out.Sides = s.liveSides()
		out.ElapsedNanos = time.Since(s.startedAt).Nanoseconds()
	}
	return out
}

// countingRoundTripper wraps an HTTP transport to record transfer stats
// and feed per-side byte counters consumed by the live progress ticker.
type countingRoundTripper struct {
	base  http.RoundTripper
	label string
	stats *statsCollector
}

func (rt *countingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	side := rt.stats.side(rt.label)

	// Wrap the request body so upload bytes (receive-pack POSTs) feed
	// the per-side counter as they stream out, not just on round-trip
	// completion. Skipped when there is no body (most GETs).
	if req.Body != nil {
		req.Body = &countingReadCloser{ReadCloser: req.Body, side: side}
	}

	res, err := rt.base.RoundTrip(req)
	if err != nil {
		return nil, fmt.Errorf("round trip: %w", err)
	}

	serviceName := req.Header.Get(gitproto.StatsPhaseHeader)
	if serviceName == "" {
		serviceName = req.URL.Query().Get("service")
		if serviceName == "" {
			serviceName = strings.TrimPrefix(req.URL.Path[strings.LastIndex(req.URL.Path, "/")+1:], "/")
		}
	}
	name := strings.TrimSpace(rt.label + " " + serviceName)
	requestBytes := req.ContentLength
	if requestBytes < 0 {
		requestBytes = 0
	}

	res.Body = &countingReadCloser{
		ReadCloser: res.Body,
		side:       side,
		onClose: func(n int64) {
			rt.stats.recordRoundTrip(name, requestBytes, n)
		},
	}
	return res, nil
}

type countingReadCloser struct {
	io.ReadCloser

	n       int64
	side    *sideCounter
	onClose func(int64)
}

func (c *countingReadCloser) Read(p []byte) (int, error) {
	n, err := c.ReadCloser.Read(p)
	if n > 0 {
		c.n += int64(n)
		if c.side != nil {
			c.side.bytes.Add(int64(n))
			c.side.lastByteAt.Store(time.Now().UnixNano())
		}
	}
	return n, err //nolint:wrapcheck // Read must preserve io.EOF for io.Reader contract
}

func (c *countingReadCloser) Close() error {
	err := c.ReadCloser.Close()
	if c.onClose != nil {
		c.onClose(c.n)
		c.onClose = nil
	}
	if err != nil {
		return fmt.Errorf("close: %w", err)
	}
	return nil
}
