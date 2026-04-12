#!/bin/bash
set -euo pipefail

# Fast pre-check: compile the benchmarked package before spending time running benches.
go test -run '^$' ./internal/syncer >/dev/null

runs=5
pattern='BenchmarkRun(BootstrapEmptyTarget|IncrementalRelay|MaterializedFallback)$'

parse_metrics() {
  awk '
    function sort_num(a, n,    i, j, tmp) {
      for (i = 1; i <= n; i++) {
        for (j = i + 1; j <= n; j++) {
          if (a[j] < a[i]) {
            tmp = a[i]
            a[i] = a[j]
            a[j] = tmp
          }
        }
      }
    }
    function median(a, n,    mid) {
      sort_num(a, n)
      mid = int((n + 1) / 2)
      if (n % 2 == 1) return a[mid]
      return (a[mid] + a[mid + 1]) / 2
    }
    /BenchmarkRunBootstrapEmptyTarget-[0-9]+/ {
      bootstrap_ns = $3 + 0
      bootstrap_alloc_b = $5 + 0
    }
    /BenchmarkRunIncrementalRelay-[0-9]+/ {
      incremental_ns = $3 + 0
      incremental_alloc_b = $5 + 0
    }
    /BenchmarkRunMaterializedFallback-[0-9]+/ {
      materialized_ns = $3 + 0
      materialized_alloc_b = $5 + 0
    }
    /^ok[[:space:]]+github.com\/soph\/git-sync\/internal\/syncer[[:space:]]+[0-9.]+s$/ {
      if (bootstrap_ns > 0 && incremental_ns > 0 && materialized_ns > 0) {
        run_count++
        total_ns[run_count] = bootstrap_ns + incremental_ns + materialized_ns
        wall_s[run_count] = substr($3, 1, length($3) - 1) + 0
        run_bootstrap_ns[run_count] = bootstrap_ns
        run_incremental_ns[run_count] = incremental_ns
        run_materialized_ns[run_count] = materialized_ns
        run_bootstrap_alloc_b[run_count] = bootstrap_alloc_b
        run_incremental_alloc_b[run_count] = incremental_alloc_b
        run_materialized_alloc_b[run_count] = materialized_alloc_b
      }
      bootstrap_ns = incremental_ns = materialized_ns = 0
      bootstrap_alloc_b = incremental_alloc_b = materialized_alloc_b = 0
    }
    END {
      if (run_count == 0) {
        print "failed_to_parse=1" > "/dev/stderr"
        exit 2
      }
      median_total_ns = median(total_ns, run_count)
      for (i = 1; i <= run_count; i++) {
        if (total_ns[i] == median_total_ns) {
          median_idx = i
          break
        }
      }
      if (median_idx == 0) median_idx = int((run_count + 1) / 2)
      printf "total_ms=%.3f\n", median_total_ns / 1000000
      printf "bootstrap_ms=%.3f\n", run_bootstrap_ns[median_idx] / 1000000
      printf "incremental_ms=%.3f\n", run_incremental_ns[median_idx] / 1000000
      printf "materialized_ms=%.3f\n", run_materialized_ns[median_idx] / 1000000
      printf "bootstrap_alloc_b=%d\n", run_bootstrap_alloc_b[median_idx]
      printf "incremental_alloc_b=%d\n", run_incremental_alloc_b[median_idx]
      printf "materialized_alloc_b=%d\n", run_materialized_alloc_b[median_idx]
      printf "go_test_elapsed_s=%.3f\n", median(wall_s, run_count)
    }
  ' "$1"
}

tmp=$(mktemp)
trap 'rm -f "$tmp"' EXIT

for _ in $(seq 1 "$runs"); do
  go test -run '^$' -bench "$pattern" -benchmem ./internal/syncer >>"$tmp"
done

metrics=$(parse_metrics "$tmp")
while IFS= read -r line; do
  key=${line%%=*}
  value=${line#*=}
  printf 'METRIC %s=%s\n' "$key" "$value"
done <<< "$metrics"
