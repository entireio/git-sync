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
// fixed interval.
//
// The visible region is at most two rows: an optional "transient" line
// above (used for in-place sideband progress like "source: Compressing
// objects: 89%") and the throughput ticker below. Each redraw uses
// cursor-up + erase-to-end-of-screen to overwrite the whole region in
// place, so '\r'-terminated sideband updates from go-git read as a
// single updating row instead of scrolling line by line.
type progressReporter struct {
	out      io.Writer
	stats    *statsCollector
	interval time.Duration
	start    time.Time

	stopOnce sync.Once
	stop     chan struct{}
	done     chan struct{}

	mu        sync.Mutex
	rowsDrawn int    // rows currently occupying the live region
	lastLine  string // last progress line, kept so setTransient can redraw without re-sampling
	transient string // current sideband-progress line, "" when none
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

// ANSI control sequences we use to redraw the live region in place.
// J erases from the cursor to the end of the screen (covers both rows
// when the transient is shown); %dA moves the cursor up that many
// lines. Both are widely supported.
const clearDown = "\x1b[J"

// cursorUpToTopLocked positions the cursor at column 0 of the top row
// of the currently-drawn live region. Always emits '\r' so the first
// render (rowsDrawn=0) still starts at column 0 instead of writing in
// the middle of whatever line the cursor was last on. Caller must hold
// p.mu.
func (p *progressReporter) cursorUpToTopLocked() {
	if p.rowsDrawn > 1 {
		fmt.Fprintf(p.out, "\x1b[%dA", p.rowsDrawn-1)
	}
	fmt.Fprint(p.out, "\r")
}

// drawLocked rewrites the live region in place: cursor jumps to the top
// row, erases everything below, then writes transient (if any) followed
// by the throughput line. Cursor is left at the end of the throughput
// row so subsequent ticker frames see rowsDrawn=region-height.
func (p *progressReporter) drawLocked(line string) {
	p.cursorUpToTopLocked()
	fmt.Fprint(p.out, clearDown)

	rows := 0
	if p.transient != "" {
		fmt.Fprintln(p.out, p.transient)
		rows++
	}
	if line != "" {
		fmt.Fprint(p.out, line)
		rows++
	}
	p.rowsDrawn = rows
	p.lastLine = line
}

// notify writes a one-time permanent message above the live region.
// The region is cleared first so the message lands on a clean row,
// rowsDrawn is reset so the next render redraws the ticker below, and
// the transient slot is cleared — a permanent line typically marks a
// state transition (sideband phase completion, slog event, subdivision
// notice) where the previously-shown transient progress is no longer
// the latest activity. Safe to call concurrently with the ticker.
func (p *progressReporter) notify(msg string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cursorUpToTopLocked()
	fmt.Fprint(p.out, clearDown)
	fmt.Fprintln(p.out, msg)
	p.rowsDrawn = 0
	p.transient = ""
}

// setTransient updates the in-place sideband row above the ticker.
// Pass "" to clear it. Triggers an immediate redraw so '\r'-driven
// progress (Compressing/Counting/Resolving) feels responsive between
// ticker intervals.
func (p *progressReporter) setTransient(line string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.transient = line
	p.drawLocked(p.lastLine)
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
		if p.rowsDrawn > 0 {
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
			b.WriteString(sideSeparator)
		}
		b.WriteString(formatSide(side, elapsed, final))
	}
	// Surface the current activity label (e.g. "pack 3/8") on live
	// frames only. The final frame is implicitly "done" — appending
	// the last in-progress phase there would read as still-running.
	if !final {
		if phase := p.stats.getPhase(); phase != "" {
			b.WriteString("  (")
			b.WriteString(phase)
			b.WriteString(")")
		}
	}
	line := b.String()

	p.mu.Lock()
	defer p.mu.Unlock()
	p.drawLocked(line)
}

const (
	sideSeparator    = "  │  "
	flowArrow        = " → "
	doneMark         = " ✓"
	maxHostnameWidth = 30
	idleThreshold    = 750 * time.Millisecond
)

// formatSide renders a single side as host + bytes + rate, with a flow
// arrow positioned to indicate direction: source on the left of its
// counter, target on the right of its counter. Sides with neither label
// fall back to "name: bytes @ rate".
//
// The displayed rate is computed against the side's active window
// (start → last byte) when known, so once a transfer ends the number
// freezes at the actual transfer rate instead of decaying as the wall
// clock keeps ticking. forceDone (set on the final render) and a
// per-side idle gap >idleThreshold append a "✓" marker.
func formatSide(side SideBytes, fallbackDur time.Duration, forceDone bool) string {
	name := side.Display
	if name == "" {
		name = side.Label
	}
	name = truncateHost(name, maxHostnameWidth)

	rateDur := fallbackDur
	if side.ActiveNanos > 0 {
		rateDur = time.Duration(side.ActiveNanos)
	}
	rate := formatBytes(side.Bytes) + " @ " + formatRate(side.Bytes, rateDur)

	done := side.Bytes > 0 && (forceDone || time.Duration(side.IdleNanos) >= idleThreshold)
	if done {
		rate += doneMark
	}

	switch side.Label {
	case "source":
		return name + flowArrow + rate
	case "target":
		return rate + flowArrow + name
	default:
		return name + ": " + rate
	}
}

// truncateHost shortens long hostnames while keeping the apex domain
// visible. Returns the original string if it already fits within width,
// otherwise preserves the trailing two dotted labels (e.g. "cloudflare.net")
// and spends the remaining budget on a prefix from the leading subdomain.
// Falls back to right-truncation when the apex alone doesn't fit.
func truncateHost(host string, width int) string {
	if width <= 0 || len(host) <= width {
		return host
	}
	labels := strings.Split(host, ".")
	if len(labels) >= 2 {
		apex := labels[len(labels)-2] + "." + labels[len(labels)-1]
		if len(apex)+2 <= width { // room for at least one prefix char + ellipsis
			prefixBudget := width - 1 - len(apex)
			return host[:prefixBudget] + "…" + apex
		}
	}
	if width <= 1 {
		return "…"
	}
	return host[:width-1] + "…"
}

// sessionStderr is an io.Writer that hands writes to the live progress
// reporter when one is attached to the syncSession, so verbose slog
// lines and server-side sideband progress ("Resolving deltas …") land
// above the in-place ticker frame instead of clobbering it. Falls back
// to os.Stderr when no reporter is active.
//
// Partial-line writes are buffered until a '\n' or '\r' terminator
// arrives. This matters for prefixedLineWriter, which writes a logical
// line in two calls — first the prefix ("source: "), then the content
// with terminator — and would otherwise produce two separate notify
// frames split mid-line. Use as a pointer (the buffer is stateful).
type sessionStderr struct {
	s   *syncSession
	buf strings.Builder
}

func (w *sessionStderr) Write(b []byte) (int, error) {
	if w.s == nil || w.s.progress == nil {
		n, err := os.Stderr.Write(b)
		if err != nil {
			return n, fmt.Errorf("stderr write: %w", err)
		}
		return n, nil
	}
	s := string(b)
	for s != "" {
		i := strings.IndexAny(s, "\r\n")
		if i < 0 {
			w.buf.WriteString(s)
			break
		}
		w.buf.WriteString(s[:i])
		line := w.buf.String()
		w.buf.Reset()
		// '\r' marks an in-place sideband update (git's
		// "Compressing 89%\r" → "Compressing 90%\r" pattern); '\n'
		// marks a permanent line that scrolls. Route accordingly so
		// percentage updates rewrite a single transient row instead
		// of filling the scrollback.
		if line != "" {
			if s[i] == '\r' {
				w.s.progress.setTransient(line)
			} else {
				w.s.progress.notify(line)
			}
		}
		s = s[i+1:]
	}
	return len(b), nil
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
