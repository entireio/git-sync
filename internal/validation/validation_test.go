package validation

import (
	"testing"

	"github.com/go-git/go-git/v6/plumbing"
)

func TestNormalizeProtocolMode(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{name: "default", input: "", want: ProtocolAuto},
		{name: "auto", input: ProtocolAuto, want: ProtocolAuto},
		{name: "v1", input: ProtocolV1, want: ProtocolV1},
		{name: "v2", input: ProtocolV2, want: ProtocolV2},
		{name: "invalid", input: "v3", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NormalizeProtocolMode(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeProtocolMode: %v", err)
			}
			if got != tt.want {
				t.Fatalf("NormalizeProtocolMode(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestParseMapping(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantSrc string
		wantDst string
		wantErr bool
	}{
		{name: "short refs", input: "main:stable", wantSrc: "main", wantDst: "stable"},
		{name: "trim whitespace", input: " refs/heads/main : refs/heads/stable ", wantSrc: "refs/heads/main", wantDst: "refs/heads/stable"},
		{name: "missing colon", input: "main", wantErr: true},
		{name: "empty source", input: ":stable", wantErr: true},
		{name: "empty target", input: "main:", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseMapping(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseMapping: %v", err)
			}
			if got.Source != tt.wantSrc || got.Target != tt.wantDst {
				t.Fatalf("ParseMapping(%q) = %+v, want source=%q target=%q", tt.input, got, tt.wantSrc, tt.wantDst)
			}
		})
	}
}

func TestParseHaveRef(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  plumbing.ReferenceName
	}{
		{name: "short branch", input: "main", want: plumbing.NewBranchReferenceName("main")},
		{name: "trim short branch", input: " main ", want: plumbing.NewBranchReferenceName("main")},
		{name: "fully qualified", input: "refs/tags/v1.0.0", want: plumbing.ReferenceName("refs/tags/v1.0.0")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ParseHaveRef(tt.input); got != tt.want {
				t.Fatalf("ParseHaveRef(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
