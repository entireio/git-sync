package syncer

import (
	"runtime"
	"sync"
	"time"
)

// Measurement holds performance measurement data.
type Measurement struct {
	Enabled            bool   `json:"enabled"`
	ElapsedMillis      int64  `json:"elapsedMillis"`
	PeakAllocBytes     uint64 `json:"peakAllocBytes"`
	PeakHeapInuseBytes uint64 `json:"peakHeapInuseBytes"`
	TotalAllocBytes    uint64 `json:"totalAllocBytes"`
	GCCount            uint32 `json:"gcCount"`
}

func startMeasurement(enabled bool) func() Measurement {
	if !enabled {
		return func() Measurement { return Measurement{} }
	}

	start := time.Now()
	var startStats runtime.MemStats
	runtime.ReadMemStats(&startStats)

	done := make(chan struct{})
	var (
		mu            sync.Mutex
		peakAlloc     = startStats.Alloc
		peakHeapInuse = startStats.HeapInuse
		result        Measurement
	)

	go func() {
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				var current runtime.MemStats
				runtime.ReadMemStats(&current)
				mu.Lock()
				if current.Alloc > peakAlloc {
					peakAlloc = current.Alloc
				}
				if current.HeapInuse > peakHeapInuse {
					peakHeapInuse = current.HeapInuse
				}
				mu.Unlock()
			}
		}
	}()

	var once sync.Once
	return func() Measurement {
		once.Do(func() {
			close(done)
			var endStats runtime.MemStats
			runtime.ReadMemStats(&endStats)
			mu.Lock()
			if endStats.Alloc > peakAlloc {
				peakAlloc = endStats.Alloc
			}
			if endStats.HeapInuse > peakHeapInuse {
				peakHeapInuse = endStats.HeapInuse
			}
			result = Measurement{
				Enabled:            true,
				ElapsedMillis:      time.Since(start).Milliseconds(),
				PeakAllocBytes:     peakAlloc,
				PeakHeapInuseBytes: peakHeapInuse,
				TotalAllocBytes:    endStats.TotalAlloc - startStats.TotalAlloc,
				GCCount:            endStats.NumGC - startStats.NumGC,
			}
			mu.Unlock()
		})
		return result
	}
}
