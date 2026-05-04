package syncer

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// progressReporter renders live per-side throughput to a writer (typically
// os.Stderr) by sampling the statsCollector's atomic byte counters on a
// fixed interval. It is intentionally a one-line in-place renderer so it
// stays out of the way of the final command output.
type progressReporter struct {
	out      io.Writer
	stats    *statsCollector
	interval time.Duration
	start    time.Time

	stopOnce sync.Once
	stop     chan struct{}
	done     chan struct{}

	mu      sync.Mutex
	lastLen int
}

func newProgressReporter(out io.Writer, stats *statsCollector, interval time.Duration) *progressReporter {
	if interval <= 0 {
		interval = 200 * time.Millisecond
	}
	return &progressReporter{
		out:      out,
		stats:    stats,
		interval: interval,
		start:    time.Now(),
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
}

// run drives the ticker until stop() is called. Safe to call once.
func (p *progressReporter) run() {
	defer close(p.done)
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	for {
		select {
		case <-p.stop:
			return
		case <-ticker.C:
			p.render(false)
		}
	}
}

// terminate halts the ticker, draws one final frame so the printed line
// reflects the closing byte counts, and emits a newline so subsequent
// command output starts on a fresh row.
func (p *progressReporter) terminate() {
	p.stopOnce.Do(func() {
		close(p.stop)
		<-p.done
		p.render(true)
		p.mu.Lock()
		if p.lastLen > 0 {
			fmt.Fprintln(p.out)
		}
		p.mu.Unlock()
	})
}

func (p *progressReporter) render(final bool) {
	sides := p.stats.liveSides()
	if len(sides) == 0 && !final {
		return
	}
	sort.Slice(sides, func(i, j int) bool { return sides[i].Label < sides[j].Label })
	elapsed := time.Since(p.start)

	var b strings.Builder
	for i, side := range sides {
		if i > 0 {
			b.WriteString(" · ")
		}
		fmt.Fprintf(&b, "%s: %s @ %s",
			side.Label, formatBytes(side.Bytes), formatRate(side.Bytes, elapsed))
	}
	line := b.String()

	p.mu.Lock()
	defer p.mu.Unlock()
	pad := 0
	if p.lastLen > len(line) {
		pad = p.lastLen - len(line)
	}
	fmt.Fprint(p.out, "\r"+line+strings.Repeat(" ", pad))
	p.lastLen = len(line)
}

// stderrIsTTY reports whether stderr is attached to a terminal. The
// progress ticker is suppressed otherwise because '\r' updates only make
// sense on a TTY and would otherwise spam log files.
func stderrIsTTY() bool {
	fi, err := os.Stderr.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

// formatBytes renders byte counts in IEC-ish human units (binary base).
func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	value := float64(n) / float64(div)
	suffix := []string{"KB", "MB", "GB", "TB", "PB"}[exp]
	if value >= 100 {
		return fmt.Sprintf("%.0f %s", value, suffix)
	}
	if value >= 10 {
		return fmt.Sprintf("%.1f %s", value, suffix)
	}
	return fmt.Sprintf("%.2f %s", value, suffix)
}

// formatRate renders a bytes/second average over the supplied duration.
// Returns "0 B/s" until the duration is large enough to be meaningful,
// avoiding misleadingly large rates from sub-millisecond samples.
func formatRate(bytes int64, dur time.Duration) string {
	if dur < 50*time.Millisecond || bytes <= 0 {
		return "0 B/s"
	}
	rate := float64(bytes) / dur.Seconds()
	return formatBytes(int64(rate)) + "/s"
}
