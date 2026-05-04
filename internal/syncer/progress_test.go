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
	stats.side("source").bytes.Store(2 * 1024 * 1024)
	stats.side("target").bytes.Store(1024 * 1024)

	var buf bytes.Buffer
	p := newProgressReporter(&buf, stats, 0)
	p.render(true)

	out := buf.String()
	if !strings.Contains(out, "source: 2.00 MB") {
		t.Errorf("output missing source bytes: %q", out)
	}
	if !strings.Contains(out, "target: 1.00 MB") {
		t.Errorf("output missing target bytes: %q", out)
	}
	if !strings.HasPrefix(out, "\r") {
		t.Errorf("output should start with carriage return for in-place updates: %q", out)
	}
	if !strings.Contains(out, "·") {
		t.Errorf("output should separate sides with a middle dot: %q", out)
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
			{Label: "target", Bytes: 1 << 20},
			{Label: "source", Bytes: 4 << 20},
		},
	}
	line := throughputLine(stats)
	if !strings.HasPrefix(line, "throughput: ") {
		t.Fatalf("line should start with 'throughput: ', got %q", line)
	}
	// Sides must be ordered alphabetically so output is stable across runs.
	sourceIdx := strings.Index(line, "source=")
	targetIdx := strings.Index(line, "target=")
	if sourceIdx < 0 || targetIdx < 0 || sourceIdx > targetIdx {
		t.Errorf("expected source before target in %q", line)
	}
	if !strings.Contains(line, "4.00 MB") || !strings.Contains(line, "4.00 MB/s") {
		t.Errorf("source 4 MiB / 1s should render as 4.00 MB and 4.00 MB/s, got %q", line)
	}
}

func TestThroughputLineEmptyWhenNoBytes(t *testing.T) {
	t.Parallel()
	stats := Stats{Enabled: true, ElapsedNanos: time.Second.Nanoseconds()}
	if got := throughputLine(stats); got != "" {
		t.Errorf("expected empty line with no sides, got %q", got)
	}
}
