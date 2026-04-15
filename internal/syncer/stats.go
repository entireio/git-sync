package syncer

import (
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/entirehq/git-sync/internal/gitproto"
)

// ServiceStats tracks transfer statistics for a single service.
type ServiceStats struct {
	Name          string `json:"name"`
	Requests      int    `json:"requests"`
	RequestBytes  int64  `json:"request_bytes"`
	ResponseBytes int64  `json:"response_bytes"`
	Wants         int    `json:"wants"`
	Haves         int    `json:"haves"`
	Commands      int    `json:"commands"`
}

// Stats holds the collected transfer statistics.
type Stats struct {
	Enabled bool                     `json:"enabled"`
	Items   map[string]*ServiceStats `json:"items"`
}

// statsCollector is a concurrency-safe stats collector (issue #8).
type statsCollector struct {
	enabled bool
	mu      sync.Mutex
	items   map[string]*ServiceStats
}

func newStats(enabled bool) *statsCollector {
	return &statsCollector{enabled: enabled, items: map[string]*ServiceStats{}}
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

func (s *statsCollector) snapshot() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := Stats{Enabled: s.enabled, Items: make(map[string]*ServiceStats, len(s.items))}
	for key, item := range s.items {
		copyItem := *item
		out.Items[key] = &copyItem
	}
	return out
}


// countingRoundTripper wraps an HTTP transport to record transfer stats.
type countingRoundTripper struct {
	base  http.RoundTripper
	label string
	stats *statsCollector
}

func (rt *countingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	res, err := rt.base.RoundTrip(req)
	if err != nil {
		return nil, err
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
		onClose: func(n int64) {
			rt.stats.recordRoundTrip(name, requestBytes, n)
		},
	}
	return res, nil
}

type countingReadCloser struct {
	io.ReadCloser
	n       int64
	onClose func(int64)
}

func (c *countingReadCloser) Read(p []byte) (int, error) {
	n, err := c.ReadCloser.Read(p)
	c.n += int64(n)
	return n, err
}

func (c *countingReadCloser) Close() error {
	err := c.ReadCloser.Close()
	if c.onClose != nil {
		c.onClose(c.n)
		c.onClose = nil
	}
	return err
}
