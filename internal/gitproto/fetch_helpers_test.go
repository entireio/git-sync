package gitproto

import (
	"testing"

	"github.com/go-git/go-git/v6/plumbing"
)

func TestCollectWants(t *testing.T) {
	hashA := plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	hashB := plumbing.NewHash("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")

	tests := []struct {
		name    string
		desired map[plumbing.ReferenceName]DesiredRef
		want    []plumbing.Hash
	}{
		{
			name:    "empty map",
			desired: map[plumbing.ReferenceName]DesiredRef{},
			want:    []plumbing.Hash{},
		},
		{
			name: "single ref",
			desired: map[plumbing.ReferenceName]DesiredRef{
				"refs/heads/main": {SourceHash: hashA},
			},
			want: []plumbing.Hash{hashA},
		},
		{
			name: "deduplicated and sorted",
			desired: map[plumbing.ReferenceName]DesiredRef{
				"refs/heads/main": {SourceHash: hashB},
				"refs/heads/dev":  {SourceHash: hashA},
				"refs/heads/dup":  {SourceHash: hashB}, // duplicate of main
			},
			want: []plumbing.Hash{hashA, hashB}, // sorted: aa < bb
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := collectWants(tt.desired)
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

func TestHasTag(t *testing.T) {
	hashA := plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")

	tests := []struct {
		name    string
		desired map[plumbing.ReferenceName]DesiredRef
		want    bool
	}{
		{
			name:    "empty map",
			desired: map[plumbing.ReferenceName]DesiredRef{},
			want:    false,
		},
		{
			name: "no tags",
			desired: map[plumbing.ReferenceName]DesiredRef{
				"refs/heads/main": {SourceHash: hashA, IsTag: false},
			},
			want: false,
		},
		{
			name: "has tag",
			desired: map[plumbing.ReferenceName]DesiredRef{
				"refs/heads/main":  {SourceHash: hashA, IsTag: false},
				"refs/tags/v1.0.0": {SourceHash: hashA, IsTag: true},
			},
			want: true,
		},
		{
			name: "only tags",
			desired: map[plumbing.ReferenceName]DesiredRef{
				"refs/tags/v1.0.0": {SourceHash: hashA, IsTag: true},
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := hasTag(tt.desired)
			if got != tt.want {
				t.Errorf("hasTag() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRefValues(t *testing.T) {
	hashA := plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	hashB := plumbing.NewHash("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")

	tests := []struct {
		name string
		m    map[plumbing.ReferenceName]plumbing.Hash
		want int // expected count of non-zero hashes
	}{
		{
			name: "empty map",
			m:    map[plumbing.ReferenceName]plumbing.Hash{},
			want: 0,
		},
		{
			name: "all non-zero",
			m: map[plumbing.ReferenceName]plumbing.Hash{
				"refs/heads/main": hashA,
				"refs/heads/dev":  hashB,
			},
			want: 2,
		},
		{
			name: "some zero",
			m: map[plumbing.ReferenceName]plumbing.Hash{
				"refs/heads/main":    hashA,
				"refs/heads/new":     plumbing.ZeroHash,
				"refs/heads/another": hashB,
			},
			want: 2,
		},
		{
			name: "all zero",
			m: map[plumbing.ReferenceName]plumbing.Hash{
				"refs/heads/a": plumbing.ZeroHash,
				"refs/heads/b": plumbing.ZeroHash,
			},
			want: 0,
		},
		{
			name: "nil map",
			m:    nil,
			want: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := refValues(tt.m)
			if len(got) != tt.want {
				t.Fatalf("refValues() returned %d values, want %d", len(got), tt.want)
			}
			// Verify none of the returned hashes are zero.
			for _, h := range got {
				if h.IsZero() {
					t.Errorf("refValues() returned zero hash, should be excluded")
				}
			}
		})
	}
}
