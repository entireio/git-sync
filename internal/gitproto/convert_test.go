package gitproto

import (
	"io"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
)

func TestToPushCommands(t *testing.T) {
	hashA := plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	hashB := plumbing.NewHash("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")

	tests := []struct {
		name       string
		plan       PushPlan
		wantNew    plumbing.Hash
		wantOld    plumbing.Hash
		wantDelete bool
	}{
		{
			name: "create command",
			plan: PushPlan{
				TargetRef:  "refs/heads/main",
				TargetHash: plumbing.ZeroHash,
				SourceHash: hashA,
			},
			wantNew: hashA,
			wantOld: plumbing.ZeroHash,
		},
		{
			name: "update command",
			plan: PushPlan{
				TargetRef:  "refs/heads/main",
				TargetHash: hashA,
				SourceHash: hashB,
			},
			wantNew: hashB,
			wantOld: hashA,
		},
		{
			name: "delete command",
			plan: PushPlan{
				TargetRef:  "refs/heads/old-branch",
				TargetHash: hashA,
				Delete:     true,
			},
			wantOld:    hashA,
			wantDelete: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmds := ToPushCommands([]PushPlan{tt.plan})
			if len(cmds) != 1 {
				t.Fatalf("expected 1 command, got %d", len(cmds))
			}
			cmd := cmds[0]
			if cmd.Name != tt.plan.TargetRef {
				t.Errorf("Name = %s, want %s", cmd.Name, tt.plan.TargetRef)
			}
			if cmd.Old != tt.wantOld {
				t.Errorf("Old = %s, want %s", cmd.Old, tt.wantOld)
			}
			if cmd.Delete != tt.wantDelete {
				t.Errorf("Delete = %v, want %v", cmd.Delete, tt.wantDelete)
			}
			if !tt.wantDelete && cmd.New != tt.wantNew {
				t.Errorf("New = %s, want %s", cmd.New, tt.wantNew)
			}
		})
	}
}

func TestToPushCommandsMultiple(t *testing.T) {
	plans := []PushPlan{
		{TargetRef: "refs/heads/a", SourceHash: plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")},
		{TargetRef: "refs/heads/b", SourceHash: plumbing.NewHash("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")},
		{TargetRef: "refs/heads/c", TargetHash: plumbing.NewHash("cccccccccccccccccccccccccccccccccccccccc"), Delete: true},
	}
	cmds := ToPushCommands(plans)
	if len(cmds) != 3 {
		t.Fatalf("expected 3 commands, got %d", len(cmds))
	}
}

func TestToPushCommandsEmpty(t *testing.T) {
	cmds := ToPushCommands(nil)
	if len(cmds) != 0 {
		t.Fatalf("expected 0 commands for nil input, got %d", len(cmds))
	}
}

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
