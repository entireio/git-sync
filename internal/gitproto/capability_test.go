package gitproto

import (
	"testing"

	"github.com/go-git/go-git/v6/plumbing/protocol/packp/capability"
	"github.com/stretchr/testify/require"
)

func TestV2CapabilitiesFetchSupports(t *testing.T) {
	caps := &V2Capabilities{
		Caps: map[string]string{
			"fetch": "thin-pack filter",
			"agent": "git/test",
		},
	}

	tests := []struct {
		feature string
		want    bool
	}{
		{"filter", true},
		{"thin-pack", true},
		{"missing", false},
		{"thin", false},
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.feature, func(t *testing.T) {
			got := caps.FetchSupports(tt.feature)
			if got != tt.want {
				t.Errorf("FetchSupports(%q) = %v, want %v", tt.feature, got, tt.want)
			}
		})
	}

	// nil receiver should always return false.
	var nilCaps *V2Capabilities
	if nilCaps.FetchSupports("filter") {
		t.Error("nil V2Capabilities.FetchSupports should return false")
	}
}

func TestV2CapabilitiesSortedKeys(t *testing.T) {
	caps := &V2Capabilities{
		Caps: map[string]string{
			"fetch":   "shallow",
			"ls-refs": "",
			"agent":   "git/test",
		},
	}

	got := caps.SortedKeys()
	want := []string{
		"agent=git/test",
		"fetch=shallow",
		"ls-refs",
	}
	if len(got) != len(want) {
		t.Fatalf("SortedKeys() returned %d items, want %d", len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("SortedKeys()[%d] = %q, want %q", i, got[i], want[i])
		}
	}

	// nil receiver should return nil.
	var nilCaps *V2Capabilities
	if keys := nilCaps.SortedKeys(); keys != nil {
		t.Errorf("nil V2Capabilities.SortedKeys() = %v, want nil", keys)
	}
}

func TestV2CapabilitiesSupportsAndValue(t *testing.T) {
	caps := &V2Capabilities{
		Caps: map[string]string{
			"fetch":   "shallow",
			"ls-refs": "",
		},
	}

	if !caps.Supports("fetch") {
		t.Error("expected Supports(fetch) = true")
	}
	if !caps.Supports("ls-refs") {
		t.Error("expected Supports(ls-refs) = true")
	}
	if caps.Supports("push") {
		t.Error("expected Supports(push) = false")
	}
	if got := caps.Value("fetch"); got != "shallow" {
		t.Errorf("Value(fetch) = %q, want %q", got, "shallow")
	}
	if got := caps.Value("ls-refs"); got != "" {
		t.Errorf("Value(ls-refs) = %q, want empty", got)
	}
	if got := caps.Value("push"); got != "" {
		t.Errorf("Value(push) = %q, want empty", got)
	}

	// nil receiver tests.
	var nilCaps *V2Capabilities
	if nilCaps.Supports("fetch") {
		t.Error("nil V2Capabilities.Supports should return false")
	}
	if got := nilCaps.Value("fetch"); got != "" {
		t.Errorf("nil V2Capabilities.Value should return empty, got %q", got)
	}
}

func TestPreferredSideband(t *testing.T) {
	tests := []struct {
		name string
		caps []capability.Capability
		want capability.Capability
	}{
		{
			name: "both supported prefers 64k",
			caps: []capability.Capability{capability.Sideband, capability.Sideband64k},
			want: capability.Sideband64k,
		},
		{
			name: "only sideband",
			caps: []capability.Capability{capability.Sideband},
			want: capability.Sideband,
		},
		{
			name: "only sideband64k",
			caps: []capability.Capability{capability.Sideband64k},
			want: capability.Sideband64k,
		},
		{
			name: "neither supported",
			caps: []capability.Capability{capability.NoProgress},
			want: "",
		},
		{
			name: "empty capabilities",
			caps: nil,
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			list := capability.NewList()
			for _, c := range tt.caps {
				require.NoError(t, list.Set(c))
			}
			got := PreferredSideband(list)
			if got != tt.want {
				t.Errorf("PreferredSideband() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRequestCapabilities(t *testing.T) {
	// With agent set, RequestCapabilities should return an agent line.
	caps := &V2Capabilities{
		Caps: map[string]string{
			"agent":   "git/test",
			"ls-refs": "",
		},
	}
	got := caps.RequestCapabilities()
	if len(got) != 1 {
		t.Fatalf("RequestCapabilities() returned %d items, want 1", len(got))
	}
	if got[0] != "agent="+capability.DefaultAgent() {
		t.Errorf("RequestCapabilities()[0] = %q, want %q", got[0], "agent="+capability.DefaultAgent())
	}

	// Without agent, RequestCapabilities should return empty.
	capsNoAgent := &V2Capabilities{
		Caps: map[string]string{
			"ls-refs": "",
			"fetch":   "shallow",
		},
	}
	got = capsNoAgent.RequestCapabilities()
	if len(got) != 0 {
		t.Errorf("RequestCapabilities() without agent returned %d items, want 0", len(got))
	}
}
