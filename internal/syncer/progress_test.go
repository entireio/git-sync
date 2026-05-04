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
	if !strings.HasPrefix(out, "\r") {
		t.Errorf("output should start with carriage return for in-place updates: %q", out)
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
