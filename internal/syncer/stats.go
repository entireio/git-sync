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
type SideBytes struct {
	Label string `json:"label"`
	Bytes int64  `json:"bytes"`
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
type sideCounter struct {
	bytes atomic.Int64
}

// statsCollector is a concurrency-safe stats collector.
type statsCollector struct {
	enabled   bool
	startedAt time.Time
	mu        sync.Mutex
	items     map[string]*ServiceStats
	sidesMu   sync.RWMutex
	sides     map[string]*sideCounter
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

// liveSides returns a snapshot of per-side byte totals for live rendering.
func (s *statsCollector) liveSides() []SideBytes {
	s.sidesMu.RLock()
	defer s.sidesMu.RUnlock()
	out := make([]SideBytes, 0, len(s.sides))
	for label, sc := range s.sides {
		out = append(out, SideBytes{Label: label, Bytes: sc.bytes.Load()})
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
