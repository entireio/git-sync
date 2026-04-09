package syncer

import (
	"context"
	"os"
	"testing"
)

const liveLinuxEnv = "GITSYNC_E2E_LIVE_LINUX"

func TestFetch_LiveLinuxSource(t *testing.T) {
	if os.Getenv(liveLinuxEnv) == "" {
		t.Skip("set GITSYNC_E2E_LIVE_LINUX=1 to run the live linux source smoke test")
	}

	result, err := Fetch(context.Background(), Config{
		Source: Endpoint{URL: "https://github.com/torvalds/linux.git"},
		Branches: []string{"master"},
		ProtocolMode: protocolModeAuto,
		ShowStats: true,
		MeasureMemory: true,
	}, nil, nil)
	if err != nil {
		t.Fatalf("live linux fetch failed: %v", err)
	}
	if result.Protocol == "" {
		t.Fatalf("expected negotiated protocol, got empty result")
	}
	if len(result.Wants) == 0 {
		t.Fatalf("expected at least one wanted ref")
	}
	if !result.Measurement.Enabled {
		t.Fatalf("expected measurement to be enabled")
	}
}
