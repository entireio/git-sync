package syncer

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestFormatBytes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{42, "42 B"},
		{1023, "1023 B"},
		{1024, "1.00 KB"},
		{1500, "1.46 KB"},
		{int64(15 * 1024), "15.0 KB"},
		{int64(1024 * 1024), "1.00 MB"},
		{int64(150 * 1024 * 1024), "150 MB"},
	}
	for _, c := range cases {
		if got := formatBytes(c.in); got != c.want {
			t.Errorf("formatBytes(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestFormatRate(t *testing.T) {
	t.Parallel()
	const zeroRate = "0 B/s"
	if got := formatRate(0, time.Second); got != zeroRate {
		t.Errorf("zero bytes should produce %s, got %q", zeroRate, got)
	}
	if got := formatRate(1<<20, time.Microsecond); got != zeroRate {
		t.Errorf("sub-50ms windows should produce %s to avoid bogus rates, got %q", zeroRate, got)
	}
	got := formatRate(1<<20, time.Second)
	if got != "1.00 MB/s" {
		t.Errorf("1 MiB/s should format as 1.00 MB/s, got %q", got)
	}
}

func TestProgressReporterRendersBothSides(t *testing.T) {
	t.Parallel()
	stats := newStats(true)
	stats.setSideDisplay("source", "github.com")
	stats.setSideDisplay("target", "example.test")
	stats.side("source").bytes.Store(2 * 1024 * 1024)
	stats.side("target").bytes.Store(1024 * 1024)

	var buf bytes.Buffer
	p := newProgressReporter(&buf, stats, 0)
	p.render(true)

	out := buf.String()
	if !strings.Contains(out, "github.com → 2.00 MB") {
		t.Errorf("output missing source host with arrow: %q", out)
	}
	if !strings.Contains(out, "1.00 MB @ ") || !strings.Contains(out, "→ example.test") {
		t.Errorf("output missing target host with arrow: %q", out)
	}
	// Every render starts at column 0 so it can never land mid-word; the
	// first frame may be just '\r' + clear escape, subsequent ones
	// preface with cursor-up movement when a transient row is showing.
	if !strings.HasPrefix(out, "\r") && !strings.Contains(out[:min(8, len(out))], "\x1b[") {
		t.Errorf("output should reposition cursor to column 0 before drawing: %q", out)
	}
	if !strings.Contains(out, "│") {
		t.Errorf("output should use the vertical bar separator: %q", out)
	}
	// Source must precede target so the arrows describe the data path
	// left-to-right (source on the left, target on the right).
	if strings.Index(out, "github.com") > strings.Index(out, "example.test") {
		t.Errorf("source should appear before target: %q", out)
	}
}

func TestProgressReporterTerminateEmitsNewline(t *testing.T) {
	t.Parallel()
	stats := newStats(true)
	stats.side("source").bytes.Store(2048)

	var buf bytes.Buffer
	p := newProgressReporter(&buf, stats, time.Hour) // long interval; we only render manually
	go p.run()
	p.terminate()

	if !strings.HasSuffix(buf.String(), "\n") {
		t.Errorf("terminate should leave the cursor on a fresh line, got %q", buf.String())
	}
}

func TestThroughputLineFormatsBothSides(t *testing.T) {
	t.Parallel()
	stats := Stats{
		Enabled:      true,
		Items:        map[string]*ServiceStats{},
		ElapsedNanos: time.Second.Nanoseconds(),
		Sides: []SideBytes{
			{Label: "target", Bytes: 1 << 20, Display: "example.test"},
			{Label: "source", Bytes: 4 << 20, Display: "github.com"},
		},
	}
	line := throughputLine(stats)
	if !strings.HasPrefix(line, "throughput: ") {
		t.Fatalf("line should start with 'throughput: ', got %q", line)
	}
	// Source host must precede target host so the line reads left-to-right.
	sourceIdx := strings.Index(line, "github.com")
	targetIdx := strings.Index(line, "example.test")
	if sourceIdx < 0 || targetIdx < 0 || sourceIdx > targetIdx {
		t.Errorf("expected source host before target host in %q", line)
	}
	if !strings.Contains(line, "github.com → 4.00 MB") {
		t.Errorf("source side should render as 'github.com → 4.00 MB ...': %q", line)
	}
	if !strings.Contains(line, "→ example.test") {
		t.Errorf("target side should end in '→ example.test': %q", line)
	}
	if !strings.Contains(line, "│") {
		t.Errorf("expected vertical-bar separator between sides: %q", line)
	}
}

func TestThroughputLineEmptyWhenNoBytes(t *testing.T) {
	t.Parallel()
	stats := Stats{Enabled: true, ElapsedNanos: time.Second.Nanoseconds()}
	if got := throughputLine(stats); got != "" {
		t.Errorf("expected empty line with no sides, got %q", got)
	}
}

// TestFormatSideFreezesRateAtLastByte verifies that once a side has
// gone idle, the displayed rate uses the active window (start → last
// byte) rather than the still-advancing wall clock — so a 4 MiB
// transfer that took 1s reads as 4 MB/s even when sampled 9 seconds
// later, and the side is annotated with a done marker.
func TestFormatSideFreezesRateAtLastByte(t *testing.T) {
	t.Parallel()
	side := SideBytes{
		Label:       "source",
		Display:     "github.com",
		Bytes:       4 << 20, // 4 MiB
		ActiveNanos: time.Second.Nanoseconds(),
		IdleNanos:   (idleThreshold + time.Second).Nanoseconds(),
	}
	got := formatSide(side, 10*time.Second, 0, false)
	if !strings.Contains(got, "4.00 MB/s") {
		t.Errorf("idle side should freeze rate at active-window value: %q", got)
	}
	if !strings.HasSuffix(got, doneMark) {
		t.Errorf("idle side should be marked done: %q", got)
	}
}

func TestFormatSideActiveSideHasNoDoneMark(t *testing.T) {
	t.Parallel()
	side := SideBytes{
		Label:       "target",
		Display:     "example.test",
		Bytes:       2 << 20,
		ActiveNanos: time.Second.Nanoseconds(),
		IdleNanos:   (100 * time.Millisecond).Nanoseconds(),
	}
	got := formatSide(side, time.Second, 0, false)
	if strings.Contains(got, doneMark) {
		t.Errorf("active side should not carry done marker: %q", got)
	}
}

func TestProgressReporterRendersPhase(t *testing.T) {
	t.Parallel()
	stats := newStats(true)
	stats.setSideDisplay("source", "github.com")
	stats.setSideDisplay("target", "example.test")
	stats.side("source").bytes.Store(1024)
	stats.side("target").bytes.Store(512)
	stats.setPhase("pack 3/8")

	var buf bytes.Buffer
	p := newProgressReporter(&buf, stats, 0)
	p.render(false)

	if !strings.Contains(buf.String(), "(pack 3/8)") {
		t.Errorf("live frame should include phase suffix: %q", buf.String())
	}
}

func TestProgressReporterFinalFrameOmitsPhase(t *testing.T) {
	t.Parallel()
	stats := newStats(true)
	stats.setSideDisplay("source", "github.com")
	stats.setSideDisplay("target", "example.test")
	stats.side("source").bytes.Store(1024)
	stats.side("target").bytes.Store(512)
	stats.setPhase("pack 3/8")

	var buf bytes.Buffer
	p := newProgressReporter(&buf, stats, 0)
	p.render(true)

	if strings.Contains(buf.String(), "pack 3/8") {
		t.Errorf("final frame should drop the in-flight phase: %q", buf.String())
	}
}

// TestNotifyAfterRenderClearsTheFrame asserts that bytes written to
// notify after a render include a clear-line escape before the message,
// so the slog/sideband line doesn't end up concatenated with the live
// progress frame on the user's terminal.
func TestNotifyAfterRenderClearsTheFrame(t *testing.T) {
	t.Parallel()
	stats := newStats(true)
	stats.setSideDisplay("source", "github.com")
	stats.setSideDisplay("target", "example.test")
	stats.side("source").bytes.Store(2 * 1024 * 1024)
	stats.side("target").bytes.Store(1024 * 1024)
	stats.setPhase("pack 1/4")

	var buf bytes.Buffer
	p := newProgressReporter(&buf, stats, 0)
	p.render(false)
	frameEnd := buf.Len()

	p.notify("level=INFO msg=\"bootstrap subdividing\"")

	tail := buf.String()[frameEnd:]
	// notify must emit a clear-screen-from-cursor escape before the
	// message so the previous live region (transient + ticker) is wiped,
	// plus a trailing newline so subsequent renders draw on a fresh row.
	if !strings.Contains(tail, clearDown) {
		t.Errorf("notify should clear the region via ANSI J, got %q", tail)
	}
	if !strings.HasSuffix(tail, "\n") {
		t.Errorf("notify should terminate with newline, got %q", tail)
	}
	clearIdx := strings.Index(tail, clearDown)
	msgIdx := strings.Index(tail, "level=INFO")
	if clearIdx < 0 || msgIdx < 0 || clearIdx > msgIdx {
		t.Errorf("clear must precede the message in %q", tail)
	}
}

func TestSessionStderrRoutesMultilineThroughNotify(t *testing.T) {
	t.Parallel()
	stats := newStats(true)
	stats.setSideDisplay("source", "github.com")
	stats.side("source").bytes.Store(1024)

	var buf bytes.Buffer
	p := newProgressReporter(&buf, stats, 0)
	p.render(false) // give notify something to clear

	sess := &syncSession{progress: p}
	sink := &sessionStderr{s: sess}

	// Multi-line write (e.g. a slog line followed by a sideband line)
	// must produce one notify per logical line — both '\n' and '\r' are
	// treated as line ends so sideband '\r'-driven percentage updates
	// don't clobber the live progress frame.
	if _, err := sink.Write([]byte("first line\nsecond line\rthird line\n")); err != nil {
		t.Fatalf("write: %v", err)
	}

	for _, want := range []string{"first line", "second line", "third line"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("output missing %q: %q", want, buf.String())
		}
	}
}

// TestSessionStderrBuffersPartialLines covers the prefixedLineWriter
// pattern in internal/gitproto: a logical line arrives in two writes —
// first the "source: " prefix, then the content with terminator. The
// buffered writer must combine them into one notify call instead of
// emitting the prefix on its own row.
// TestSessionStderrCRUpdatesTransient verifies that '\r'-terminated
// sideband progress (git's "Compressing 89%\r" → "Compressing 90%\r"
// pattern) goes to the transient row instead of scrolling the
// scrollback. Subsequent updates should overwrite the transient slot
// rather than each landing on a new row.
func TestSessionStderrCRUpdatesTransient(t *testing.T) {
	t.Parallel()
	stats := newStats(true)
	stats.setSideDisplay("source", "github.com")
	stats.side("source").bytes.Store(1024)

	var buf bytes.Buffer
	p := newProgressReporter(&buf, stats, 0)
	p.render(false)

	sess := &syncSession{progress: p}
	sink := &sessionStderr{s: sess}

	// Two consecutive in-place sideband updates plus one final \n line.
	if _, err := sink.Write([]byte("source: Compressing 50%\r")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := sink.Write([]byte("source: Compressing 75%\r")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := sink.Write([]byte("source: Compressing 100%, done.\n")); err != nil {
		t.Fatalf("write: %v", err)
	}

	if p.transient != "" {
		t.Errorf("permanent line should clear transient, got %q", p.transient)
	}
	out := buf.String()
	if !strings.Contains(out, "Compressing 50%") ||
		!strings.Contains(out, "Compressing 75%") ||
		!strings.Contains(out, "Compressing 100%, done.") {
		t.Errorf("all three sideband states should be in the output stream:\n%s", out)
	}
}

func TestSessionStderrBuffersPartialLines(t *testing.T) {
	t.Parallel()
	stats := newStats(true)
	stats.setSideDisplay("source", "github.com")
	stats.side("source").bytes.Store(1024)

	var buf bytes.Buffer
	p := newProgressReporter(&buf, stats, 0)
	p.render(false)

	sess := &syncSession{progress: p}
	sink := &sessionStderr{s: sess}

	if _, err := sink.Write([]byte("source: ")); err != nil {
		t.Fatalf("prefix write: %v", err)
	}
	if _, err := sink.Write([]byte("Counting objects: 10%\r")); err != nil {
		t.Fatalf("content write: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "source: Counting objects: 10%") {
		t.Errorf("expected joined line 'source: Counting objects: 10%%', got %q", out)
	}
	// Reject the bug shape: prefix on its own line followed by content
	// on a separate line.
	if strings.Contains(out, "source: \n") || strings.Contains(out, "source:\n") {
		t.Errorf("prefix should not be emitted as a standalone line: %q", out)
	}
}

func TestProgressReporterNotifyClearsAndRedraws(t *testing.T) {
	t.Parallel()
	stats := newStats(true)
	stats.setSideDisplay("source", "github.com")
	stats.side("source").bytes.Store(1024)

	var buf bytes.Buffer
	p := newProgressReporter(&buf, stats, 0)

	// Draw a frame so the reporter has a non-zero lastLen to clear.
	p.render(false)
	beforeNotify := buf.Len()

	p.notify("target rejected pack — splitting 1 → 4 packs")

	out := buf.String()
	if !strings.Contains(out, "splitting 1 → 4 packs") {
		t.Errorf("notify output should contain the message: %q", out)
	}
	// Notify must clear the previously-drawn line so the message lands
	// on its own row instead of overlapping the progress text.
	tail := out[beforeNotify:]
	if !strings.HasPrefix(tail, "\r") {
		t.Errorf("notify should start by clearing the current line: %q", tail)
	}
	if !strings.HasSuffix(strings.TrimRight(tail, ""), "packs\n") {
		t.Errorf("notify should end with a newline so progress redraws below: %q", tail)
	}

	// The next render should redraw the progress line because lastLen
	// was reset.
	p.render(false)
	if !strings.HasSuffix(buf.String(), "github.com → 1.00 KB @ 0 B/s") &&
		!strings.Contains(buf.String()[len(out):], "github.com") {
		t.Errorf("next render after notify should redraw the progress line: %q",
			buf.String()[len(out):])
	}
}

func TestFormatSideForceDoneAlwaysMarks(t *testing.T) {
	t.Parallel()
	side := SideBytes{
		Label:       "source",
		Display:     "github.com",
		Bytes:       1024,
		ActiveNanos: time.Second.Nanoseconds(),
		IdleNanos:   0,
	}
	got := formatSide(side, time.Second, 0, true)
	if !strings.HasSuffix(got, doneMark) {
		t.Errorf("forceDone should mark the side regardless of idle gap: %q", got)
	}
}

// TestSampleRingComputesRateOverWindow walks the ring through a few
// scenarios that exercise both the partial-fill path (early in a
// transfer) and the saturated-ring path (steady state).
func TestSampleRingComputesRateOverWindow(t *testing.T) {
	t.Parallel()
	r := &sampleRing{}
	if got := r.instantRate(); got != 0 {
		t.Errorf("empty ring should report 0, got %v", got)
	}

	base := time.Now()
	r.push(sample{at: base, bytes: 0})
	if got := r.instantRate(); got != 0 {
		t.Errorf("single-sample ring should report 0, got %v", got)
	}

	// 1 MiB transferred in 1s → ~1 MiB/s.
	r.push(sample{at: base.Add(time.Second), bytes: 1 << 20})
	got := r.instantRate()
	want := float64(1 << 20)
	if got < want*0.99 || got > want*1.01 {
		t.Errorf("expected ~%v B/s, got %v", want, got)
	}

	// Saturate the ring with a steady 10 MB/s and verify the rate
	// reflects only the in-window samples (oldest is overwritten).
	r2 := &sampleRing{}
	for i := 0; i <= sampleCapacity*2; i++ {
		r2.push(sample{
			at:    base.Add(time.Duration(i) * time.Second),
			bytes: int64(i) * 10 * (1 << 20),
		})
	}
	got2 := r2.instantRate()
	want2 := float64(10 * (1 << 20))
	if got2 < want2*0.99 || got2 > want2*1.01 {
		t.Errorf("steady-state expected ~%v B/s, got %v", want2, got2)
	}
}

// TestSampleRingIdlePeriodReportsZero ensures that when bytes stop
// flowing the ring eventually returns 0 — the formatter then falls
// back to the active-window average for a stable post-transfer
// headline.
func TestSampleRingIdlePeriodReportsZero(t *testing.T) {
	t.Parallel()
	r := &sampleRing{}
	base := time.Now()
	for i := range sampleCapacity {
		r.push(sample{at: base.Add(time.Duration(i) * 200 * time.Millisecond), bytes: 1024})
	}
	if got := r.instantRate(); got != 0 {
		t.Errorf("flat byte count across ring should report 0, got %v", got)
	}
}

func TestFormatSidePrefersInstantRateWhileActive(t *testing.T) {
	t.Parallel()
	side := SideBytes{
		Label:       "source",
		Display:     "github.com",
		Bytes:       100 * (1 << 20),
		ActiveNanos: (10 * time.Second).Nanoseconds(),
		IdleNanos:   0, // active
	}
	// Active-window average = 10 MB/s. Sliding-window says 44 MB/s.
	// The displayed rate should follow the sliding window.
	got := formatSide(side, time.Second, float64(44*(1<<20)), false)
	if !strings.Contains(got, "44.0 MB/s") {
		t.Errorf("active side should show instant rate, got %q", got)
	}
}

func TestFormatSideUsesAverageWhenDone(t *testing.T) {
	t.Parallel()
	side := SideBytes{
		Label:       "source",
		Display:     "github.com",
		Bytes:       int64(2 * (1 << 20)),
		ActiveNanos: time.Second.Nanoseconds(),
		IdleNanos:   (idleThreshold + time.Second).Nanoseconds(),
	}
	// Even with a non-zero instant rate, an idle/done side must show
	// the active-window average — otherwise the post-transfer headline
	// would be a stale snapshot of pre-completion throughput.
	got := formatSide(side, time.Second, float64(99*(1<<20)), false)
	if !strings.Contains(got, "2.00 MB/s") {
		t.Errorf("idle side should fall back to average rate, got %q", got)
	}
	if !strings.HasSuffix(got, doneMark) {
		t.Errorf("idle side should still carry done marker: %q", got)
	}
}

func TestTruncateHost(t *testing.T) {
	t.Parallel()
	cases := []struct {
		host string
		max  int
		want string
	}{
		{"github.com", 30, "github.com"},
		{"a.b.c.d.example.com", 30, "a.b.c.d.example.com"},
		{
			"8b04592ed74a5cce30d355b07276caf3.artifacts.cloudflare.net",
			30,
			"8b04592ed74a5cc…cloudflare.net",
		},
		// When the apex itself doesn't fit, we fall back to right-truncation.
		{"averylongdomain.example.com", 10, "averylong…"},
		{"single", 4, "sin…"},
		{"", 30, ""},
		{"x", 0, "x"},
	}
	for _, c := range cases {
		got := truncateHost(c.host, c.max)
		if got != c.want {
			t.Errorf("truncateHost(%q, %d) = %q, want %q", c.host, c.max, got, c.want)
		}
		if c.max > 0 && len([]rune(got)) > c.max {
			t.Errorf("truncateHost(%q, %d) = %q exceeded max width %d",
				c.host, c.max, got, c.max)
		}
	}
}
