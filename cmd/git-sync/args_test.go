package main

import "testing"

// Positional endpoints must be consumed left-to-right, skipping whatever was
// already given via a flag — so `--source-url URL <target>` fills the target
// slot rather than being dropped (the reported bug across sync/bootstrap/probe).
func TestResolvePositionalEndpoints(t *testing.T) {
	const src = "https://example.invalid/source.git"
	const tgt = "https://example.invalid/target.git"

	cases := []struct {
		name                   string
		source, target         string
		args                   []string
		wantSource, wantTarget string
		wantErr                bool
	}{
		{name: "both positional", args: []string{src, tgt}, wantSource: src, wantTarget: tgt},
		{name: "source flag plus positional target", source: src, args: []string{tgt}, wantSource: src, wantTarget: tgt},
		{name: "target flag plus positional source", target: tgt, args: []string{src}, wantSource: src, wantTarget: tgt},
		{name: "both flags, no positionals", source: src, target: tgt, wantSource: src, wantTarget: tgt},
		{name: "source flag only, no positionals", source: src, wantSource: src},
		// Over-specified: source set by flag, then two positionals — the old
		// fixed-index code silently ignored one; we reject instead.
		{name: "source flag plus two positionals", source: src, args: []string{"a", "b"}, wantErr: true},
		{name: "both flags plus a positional", source: src, target: tgt, args: []string{"extra"}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			source, target := tc.source, tc.target
			err := resolvePositionalEndpoints(&source, &target, tc.args)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for over-specified args, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if source != tc.wantSource {
				t.Errorf("source = %q, want %q", source, tc.wantSource)
			}
			if target != tc.wantTarget {
				t.Errorf("target = %q, want %q", target, tc.wantTarget)
			}
		})
	}
}
