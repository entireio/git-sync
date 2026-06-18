package syncer

import (
	"runtime"
	"testing"
	"time"
)

// The measurement closure must stop its ticker goroutine when invoked, and be
// safe to invoke more than once — finish() now calls it on every path
// (including error paths) in addition to the happy-path Result builder, so a
// double-call must neither panic (double close) nor change the reported value.
func TestStartMeasurementStopsGoroutineAndIsIdempotent(t *testing.T) {
	before := runtime.NumGoroutine()

	done := startMeasurement(true)

	m1 := done()
	if !m1.Enabled {
		t.Fatalf("expected an enabled measurement, got %+v", m1)
	}
	m2 := done() // second call (the finish() path) must be a safe no-op
	if m1 != m2 {
		t.Fatalf("measurement changed across calls: %+v vs %+v", m1, m2)
	}

	// The ticker goroutine must have exited; poll to avoid races with the
	// scheduler tearing it down.
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > before {
		if time.Now().After(deadline) {
			t.Fatalf("measurement goroutine leaked: %d goroutines, baseline %d",
				runtime.NumGoroutine(), before)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// When disabled, no goroutine is started and the closure is still safe to call.
func TestStartMeasurementDisabledIsInert(t *testing.T) {
	done := startMeasurement(false)
	if m := done(); m.Enabled {
		t.Fatalf("disabled measurement should not be enabled: %+v", m)
	}
	_ = done() // idempotent
}
