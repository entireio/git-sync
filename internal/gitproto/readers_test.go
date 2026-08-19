package gitproto

import (
	"io"
	"strings"
	"testing"

	"github.com/go-git/go-git/v6/plumbing"
)

func TestLimitPackReaderWithinLimit(t *testing.T) {
	data := "hello world"
	rc := io.NopCloser(strings.NewReader(data))
	limited := LimitPackReader(rc, 1024)
	defer limited.Close()

	got, err := io.ReadAll(limited)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got) != data {
		t.Errorf("got %q, want %q", got, data)
	}
}

func TestLimitPackReaderExceedsLimit(t *testing.T) {
	data := "this is more than ten bytes of data"
	rc := io.NopCloser(strings.NewReader(data))
	limited := LimitPackReader(rc, 10)
	defer limited.Close()

	_, err := io.ReadAll(limited)
	if err == nil {
		t.Fatal("expected error when exceeding limit, got nil")
	}
	if !strings.Contains(err.Error(), "source pack exceeded max-pack-bytes limit") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestLimitPackReaderZeroLimitPassesThrough(t *testing.T) {
	data := "unlimited data"
	rc := io.NopCloser(strings.NewReader(data))
	limited := LimitPackReader(rc, 0)
	defer limited.Close()

	got, err := io.ReadAll(limited)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got) != data {
		t.Errorf("got %q, want %q", got, data)
	}
}

func TestLimitPackReaderNegativeLimitPassesThrough(t *testing.T) {
	data := "unlimited data"
	rc := io.NopCloser(strings.NewReader(data))
	limited := LimitPackReader(rc, -1)
	defer limited.Close()

	got, err := io.ReadAll(limited)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got) != data {
		t.Errorf("got %q, want %q", got, data)
	}
}

func TestSortedUniqueHashes(t *testing.T) {
	hashA := plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	hashB := plumbing.NewHash("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	hashC := plumbing.NewHash("cccccccccccccccccccccccccccccccccccccccc")

	tests := []struct {
		name  string
		input []plumbing.Hash
		want  []plumbing.Hash
	}{
		{
			name:  "deduplicates repeated hashes",
			input: []plumbing.Hash{hashA, hashB, hashA, hashC, hashB},
			want:  []plumbing.Hash{hashA, hashB, hashC},
		},
		{
			name:  "already sorted and unique is unchanged",
			input: []plumbing.Hash{hashA, hashB, hashC},
			want:  []plumbing.Hash{hashA, hashB, hashC},
		},
		{
			name:  "reverse order gets sorted",
			input: []plumbing.Hash{hashC, hashB, hashA},
			want:  []plumbing.Hash{hashA, hashB, hashC},
		},
		{
			name:  "single element",
			input: []plumbing.Hash{hashB},
			want:  []plumbing.Hash{hashB},
		},
		{
			name:  "empty input",
			input: []plumbing.Hash{},
			want:  []plumbing.Hash{},
		},
		{
			name:  "nil input",
			input: nil,
			want:  []plumbing.Hash{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SortedUniqueHashes(tt.input)
			if len(got) != len(tt.want) {
				t.Fatalf("len = %d, want %d", len(got), len(tt.want))
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("index %d: got %s, want %s", i, got[i], tt.want[i])
				}
			}
		})
	}
}

type countingCloser struct {
	io.Reader

	closes int
}

func (c *countingCloser) Close() error {
	c.closes++
	return nil
}

func TestCloseOnce(t *testing.T) {
	if CloseOnce(nil) != nil {
		t.Fatal("CloseOnce(nil) should return nil")
	}

	cc := &countingCloser{Reader: strings.NewReader("data")}
	wrapped := CloseOnce(cc)
	if again := CloseOnce(wrapped); again != wrapped {
		t.Fatal("CloseOnce should return an already-wrapped reader unchanged")
	}
	if err := wrapped.Close(); err != nil {
		t.Fatalf("Close() error: %v", err)
	}
	if err := wrapped.Close(); err != nil {
		t.Fatalf("second Close() error: %v", err)
	}
	if cc.closes != 1 {
		t.Fatalf("underlying closer closed %d times, want 1", cc.closes)
	}
}

func TestHumanBytes(t *testing.T) {
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
		{int64(3) << 30, "3.00 GB"},
		{int64(2) << 40, "2.00 TB"},
		{int64(5) << 50, "5.00 PB"},
	}
	for _, c := range cases {
		if got := HumanBytes(c.in); got != c.want {
			t.Errorf("HumanBytes(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

// zeroReader yields an endless stream of NUL bytes. Stands in for a remote that
// never stops sending: the read must terminate on the cap rather than growing
// the buffer until the process dies.
type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

func TestReadCappedAdvertisementUnderLimit(t *testing.T) {
	data, err := readCappedAdvertisement(strings.NewReader("0008abc\n0000"), "test advertisement")
	if err != nil {
		t.Fatalf("readCappedAdvertisement: %v", err)
	}
	if got, want := string(data), "0008abc\n0000"; got != want {
		t.Fatalf("data = %q, want %q", got, want)
	}
}

func TestReadCappedAdvertisementAtLimit(t *testing.T) {
	exact := io.LimitReader(zeroReader{}, MaxAdvertisementBytes)
	data, err := readCappedAdvertisement(exact, "test advertisement")
	if err != nil {
		t.Fatalf("a response exactly at the limit must be accepted: %v", err)
	}
	if int64(len(data)) != MaxAdvertisementBytes {
		t.Fatalf("read %d bytes, want %d", len(data), MaxAdvertisementBytes)
	}
}

func TestReadCappedAdvertisementRejectsEndlessStream(t *testing.T) {
	_, err := readCappedAdvertisement(zeroReader{}, "test advertisement")
	if err == nil {
		t.Fatal("expected an error for an unbounded stream, got nil")
	}
	if !strings.Contains(err.Error(), "byte limit") {
		t.Errorf("error %q does not mention the limit", err)
	}
}
