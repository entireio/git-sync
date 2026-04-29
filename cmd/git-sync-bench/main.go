package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	git "github.com/go-git/go-git/v6"

	"entire.io/git-sync/internal/validation"
	"entire.io/git-sync/pkg/gitsync"
	"entire.io/git-sync/pkg/gitsync/unstable"
)

type scenario string

const (
	scenarioBootstrap scenario = "bootstrap"
	scenarioSync      scenario = "sync"
)

type runSummary struct {
	Index      int             `json:"index"`
	TargetPath string          `json:"targetPath"`
	TargetURL  string          `json:"targetUrl"`
	WallMillis int64           `json:"wallMillis"`
	Result     unstable.Result `json:"result"`
	Error      string          `json:"error,omitempty"`
}

type aggregateSummary struct {
	SuccessfulRuns        int      `json:"successfulRuns"`
	FailedRuns            int      `json:"failedRuns"`
	BatchedRuns           int      `json:"batchedRuns"`
	MinWallMillis         int64    `json:"minWallMillis"`
	MaxWallMillis         int64    `json:"maxWallMillis"`
	AvgWallMillis         float64  `json:"avgWallMillis"`
	MinSyncElapsedMillis  int64    `json:"minSyncElapsedMillis"`
	MaxSyncElapsedMillis  int64    `json:"maxSyncElapsedMillis"`
	AvgSyncElapsedMillis  float64  `json:"avgSyncElapsedMillis"`
	MinBatchCount         int      `json:"minBatchCount,omitempty"`
	MaxBatchCount         int      `json:"maxBatchCount,omitempty"`
	AvgBatchCount         float64  `json:"avgBatchCount,omitempty"`
	MinPlannedBatchCount  int      `json:"minPlannedBatchCount,omitempty"`
	MaxPlannedBatchCount  int      `json:"maxPlannedBatchCount,omitempty"`
	AvgPlannedBatchCount  float64  `json:"avgPlannedBatchCount,omitempty"`
	MaxPeakAllocBytes     uint64   `json:"maxPeakAllocBytes"`
	MaxPeakHeapInuseBytes uint64   `json:"maxPeakHeapInuseBytes"`
	MaxTotalAllocBytes    uint64   `json:"maxTotalAllocBytes"`
	MaxGCCount            uint32   `json:"maxGcCount"`
	RelayModes            []string `json:"relayModes,omitempty"`
}

type benchmarkReport struct {
	Scenario    scenario         `json:"scenario"`
	SourceURL   string           `json:"sourceUrl"`
	Repeat      int              `json:"repeat"`
	KeepTargets bool             `json:"keepTargets"`
	WorkDir     string           `json:"workDir"`
	Config      benchmarkConfig  `json:"config"`
	Aggregate   aggregateSummary `json:"aggregate"`
	Runs        []runSummary     `json:"runs"`
}

type benchmarkConfig struct {
	SourceURL string                   `json:"sourceUrl"`
	Scope     gitsync.RefScope         `json:"scope"`
	Policy    gitsync.SyncPolicy       `json:"policy"`
	Options   unstable.AdvancedOptions `json:"options"`
}

func main() {
	if err := run(context.Background(), os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("git-sync-bench", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	var scenarioName string
	var workDir string
	var repeat int
	var keepTargets bool
	var jsonOutput bool
	var mappings multiStringFlag
	cfg := benchmarkConfig{}

	fs.StringVar(&scenarioName, "scenario", string(scenarioBootstrap), "benchmark scenario: bootstrap or sync")
	fs.StringVar(&cfg.SourceURL, "source-url", "", "source repository URL or local path")
	fs.StringVar(&workDir, "work-dir", "", "directory for temporary target repositories")
	fs.IntVar(&repeat, "repeat", 1, "number of runs to execute")
	fs.BoolVar(&keepTargets, "keep-targets", false, "retain generated target repositories after the run")
	fs.BoolVar(&jsonOutput, "json", false, "print JSON output")

	branches := fs.String("branch", "", "comma-separated branch list; default is all source branches")
	fs.Var(&mappings, "map", "ref mapping in src:dst form; short names map branches, full refs map exact refs")
	fs.BoolVar(&cfg.Policy.IncludeTags, "tags", false, "mirror tags")
	fs.BoolVar(&cfg.Policy.Force, "force", false, "allow non-fast-forward branch updates and retarget tags")
	fs.BoolVar(&cfg.Policy.Prune, "prune", false, "delete managed target refs that no longer exist on source")
	fs.BoolVar(&cfg.Options.CollectStats, "stats", false, "collect transfer statistics")
	fs.BoolVar(&cfg.Options.MeasureMemory, "measure-memory", true, "sample elapsed time and Go heap usage")
	fs.Int64Var(&cfg.Options.MaxPackBytes, "max-pack-bytes", 0, "abort bootstrap if the streamed source pack exceeds this many bytes")
	fs.Int64Var(&cfg.Options.TargetMaxPackBytes, "target-max-pack-bytes", 0, "target receive-pack body size limit; batches are planned and auto-subdivided to fit")
	benchProtocol := benchProtocolModeFlag(benchProtocolMode(validation.ProtocolAuto))
	fs.Var(&benchProtocol, "protocol", "protocol mode: auto, v1, or v2")
	fs.BoolVar(&cfg.Options.Verbose, "v", false, "verbose logging")

	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}
	cfg.Policy.Protocol = gitsync.ProtocolMode(benchProtocol)
	if len(fs.Args()) > 0 {
		return usageError("unexpected positional arguments")
	}
	if repeat < 1 {
		return usageError("--repeat must be at least 1")
	}

	if *branches != "" {
		cfg.Scope.Branches = splitCSV(*branches)
	}
	for _, raw := range mappings {
		mapping, err := validation.ParseMapping(raw)
		if err != nil {
			return fmt.Errorf("parse mapping %q: %w", raw, err)
		}
		cfg.Scope.Mappings = append(cfg.Scope.Mappings, gitsync.RefMapping{
			Source: mapping.Source,
			Target: mapping.Target,
		})
	}

	srcURL, err := normalizeRepoURL(cfg.SourceURL)
	if err != nil {
		return err
	}
	cfg.SourceURL = srcURL

	sc, err := parseScenario(scenarioName)
	if err != nil {
		return err
	}
	if sc == scenarioBootstrap {
		if cfg.Policy.Force || cfg.Policy.Prune {
			return usageError("bootstrap benchmarks do not support --force or --prune")
		}
	}

	if workDir == "" {
		workDir, err = os.MkdirTemp("", "git-sync-bench-*")
		if err != nil {
			return fmt.Errorf("create temp work dir: %w", err)
		}
	} else {
		if err := os.MkdirAll(workDir, 0o755); err != nil {
			return fmt.Errorf("create work dir: %w", err)
		}
	}

	report := benchmarkReport{
		Scenario:    sc,
		SourceURL:   cfg.SourceURL,
		Repeat:      repeat,
		KeepTargets: keepTargets,
		WorkDir:     workDir,
		Config:      cfg,
		Runs:        make([]runSummary, 0, repeat),
	}

	for i := range repeat {
		runCfg := cfg
		targetPath := filepath.Join(workDir, fmt.Sprintf("%s-run-%03d.git", sc, i+1))
		if err := os.RemoveAll(targetPath); err != nil {
			return fmt.Errorf("clear target path %s: %w", targetPath, err)
		}
		if _, err := git.PlainInit(targetPath, true); err != nil {
			return fmt.Errorf("init target repo %s: %w", targetPath, err)
		}
		targetURL, err := fileURL(targetPath)
		if err != nil {
			return err
		}
		start := time.Now()
		runResult, runErr := executeScenario(ctx, sc, runCfg, targetURL)
		summary := runSummary{
			Index:      i + 1,
			TargetPath: targetPath,
			TargetURL:  targetURL,
			WallMillis: time.Since(start).Milliseconds(),
			Result:     runResult,
		}
		if runErr != nil {
			summary.Error = runErr.Error()
		}
		report.Runs = append(report.Runs, summary)

		if !keepTargets {
			if err := os.RemoveAll(targetPath); err != nil {
				return fmt.Errorf("remove target path %s: %w", targetPath, err)
			}
		}
	}

	report.Aggregate = summarizeRuns(report.Runs)

	if jsonOutput {
		data, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal report: %w", err)
		}
		fmt.Println(string(data))
		return nil
	}

	printTextReport(report)
	return nil
}

func executeScenario(ctx context.Context, sc scenario, cfg benchmarkConfig, targetURL string) (unstable.Result, error) {
	client := unstable.New(unstable.Options{})
	switch sc {
	case scenarioBootstrap:
		result, err := client.Bootstrap(ctx, unstable.BootstrapRequest{
			Source:      gitsync.Endpoint{URL: cfg.SourceURL},
			Target:      gitsync.Endpoint{URL: targetURL},
			Scope:       cfg.Scope,
			IncludeTags: cfg.Policy.IncludeTags,
			Protocol:    cfg.Policy.Protocol,
			Options:     cfg.Options,
		})
		if err != nil {
			return unstable.Result{}, fmt.Errorf("bootstrap: %w", err)
		}
		return result, nil
	case scenarioSync:
		result, err := client.Sync(ctx, unstable.SyncRequest{
			Source:  gitsync.Endpoint{URL: cfg.SourceURL},
			Target:  gitsync.Endpoint{URL: targetURL},
			Scope:   cfg.Scope,
			Policy:  cfg.Policy,
			Options: cfg.Options,
		})
		if err != nil {
			return unstable.Result{}, fmt.Errorf("sync: %w", err)
		}
		return result, nil
	default:
		return unstable.Result{}, fmt.Errorf("unsupported scenario %q", sc)
	}
}

func summarizeRuns(runs []runSummary) aggregateSummary {
	if len(runs) == 0 {
		return aggregateSummary{}
	}

	var (
		okRuns           int
		failedRuns       int
		batchedRuns      int
		totalWall        int64
		totalSyncElapsed int64
		totalBatchCount  int
		totalPlanned     int
		relayModes       []string
	)
	summary := aggregateSummary{
		MinWallMillis:        -1,
		MinSyncElapsedMillis: -1,
		MinBatchCount:        -1,
		MinPlannedBatchCount: -1,
	}

	for _, run := range runs {
		if run.Error != "" {
			failedRuns++
			continue
		}
		okRuns++
		totalWall += run.WallMillis
		if summary.MinWallMillis < 0 || run.WallMillis < summary.MinWallMillis {
			summary.MinWallMillis = run.WallMillis
		}
		if run.WallMillis > summary.MaxWallMillis {
			summary.MaxWallMillis = run.WallMillis
		}

		m := run.Result.Measurement
		totalSyncElapsed += m.ElapsedMillis
		if summary.MinSyncElapsedMillis < 0 || m.ElapsedMillis < summary.MinSyncElapsedMillis {
			summary.MinSyncElapsedMillis = m.ElapsedMillis
		}
		if m.ElapsedMillis > summary.MaxSyncElapsedMillis {
			summary.MaxSyncElapsedMillis = m.ElapsedMillis
		}
		if m.PeakAllocBytes > summary.MaxPeakAllocBytes {
			summary.MaxPeakAllocBytes = m.PeakAllocBytes
		}
		if m.PeakHeapInuseBytes > summary.MaxPeakHeapInuseBytes {
			summary.MaxPeakHeapInuseBytes = m.PeakHeapInuseBytes
		}
		if m.TotalAllocBytes > summary.MaxTotalAllocBytes {
			summary.MaxTotalAllocBytes = m.TotalAllocBytes
		}
		if m.GCCount > summary.MaxGCCount {
			summary.MaxGCCount = m.GCCount
		}
		if mode := strings.TrimSpace(run.Result.RelayMode); mode != "" {
			relayModes = append(relayModes, mode)
		}
		if run.Result.Batching {
			batchedRuns++
			totalBatchCount += run.Result.BatchCount
			totalPlanned += run.Result.PlannedBatchCount
			if summary.MinBatchCount < 0 || run.Result.BatchCount < summary.MinBatchCount {
				summary.MinBatchCount = run.Result.BatchCount
			}
			if run.Result.BatchCount > summary.MaxBatchCount {
				summary.MaxBatchCount = run.Result.BatchCount
			}
			if summary.MinPlannedBatchCount < 0 || run.Result.PlannedBatchCount < summary.MinPlannedBatchCount {
				summary.MinPlannedBatchCount = run.Result.PlannedBatchCount
			}
			if run.Result.PlannedBatchCount > summary.MaxPlannedBatchCount {
				summary.MaxPlannedBatchCount = run.Result.PlannedBatchCount
			}
		}
	}

	summary.SuccessfulRuns = okRuns
	summary.FailedRuns = failedRuns
	summary.BatchedRuns = batchedRuns
	if okRuns > 0 {
		summary.AvgWallMillis = float64(totalWall) / float64(okRuns)
		summary.AvgSyncElapsedMillis = float64(totalSyncElapsed) / float64(okRuns)
	}
	if batchedRuns > 0 {
		summary.AvgBatchCount = float64(totalBatchCount) / float64(batchedRuns)
		summary.AvgPlannedBatchCount = float64(totalPlanned) / float64(batchedRuns)
	}
	summary.RelayModes = uniqueStrings(relayModes)
	return summary
}

func uniqueStrings(input []string) []string {
	if len(input) == 0 {
		return nil
	}
	slices.Sort(input)
	out := input[:0]
	var prev string
	for i, item := range input {
		if i == 0 || item != prev {
			out = append(out, item)
			prev = item
		}
	}
	return out
}

func normalizeRepoURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", usageError("--source-url is required")
	}
	if u, err := url.Parse(raw); err == nil && u.Scheme != "" {
		return raw, nil
	}
	return fileURL(raw)
}

func fileURL(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve path %q: %w", path, err)
	}
	return (&url.URL{Scheme: "file", Path: filepath.ToSlash(abs)}).String(), nil
}

func parseScenario(raw string) (scenario, error) {
	switch scenario(strings.TrimSpace(strings.ToLower(raw))) {
	case scenarioBootstrap:
		return scenarioBootstrap, nil
	case scenarioSync:
		return scenarioSync, nil
	default:
		return "", usageError(fmt.Sprintf("unsupported --scenario %q", raw))
	}
}

func printTextReport(report benchmarkReport) {
	fmt.Printf("scenario: %s\n", report.Scenario)
	fmt.Printf("source: %s\n", report.SourceURL)
	fmt.Printf("runs: %d success=%d failed=%d\n", report.Repeat, report.Aggregate.SuccessfulRuns, report.Aggregate.FailedRuns)
	fmt.Printf("wall-ms: avg=%.1f min=%d max=%d\n", report.Aggregate.AvgWallMillis, report.Aggregate.MinWallMillis, report.Aggregate.MaxWallMillis)
	fmt.Printf("sync-elapsed-ms: avg=%.1f min=%d max=%d\n", report.Aggregate.AvgSyncElapsedMillis, report.Aggregate.MinSyncElapsedMillis, report.Aggregate.MaxSyncElapsedMillis)
	fmt.Printf("peak-alloc-bytes: %d\n", report.Aggregate.MaxPeakAllocBytes)
	fmt.Printf("peak-heap-inuse-bytes: %d\n", report.Aggregate.MaxPeakHeapInuseBytes)
	if report.Aggregate.BatchedRuns > 0 {
		fmt.Printf("batch-count: avg=%.1f min=%d max=%d batched-runs=%d\n",
			report.Aggregate.AvgBatchCount, report.Aggregate.MinBatchCount, report.Aggregate.MaxBatchCount, report.Aggregate.BatchedRuns)
		fmt.Printf("planned-batch-count: avg=%.1f min=%d max=%d\n",
			report.Aggregate.AvgPlannedBatchCount, report.Aggregate.MinPlannedBatchCount, report.Aggregate.MaxPlannedBatchCount)
	}
	if len(report.Aggregate.RelayModes) > 0 {
		fmt.Printf("relay-modes: %s\n", strings.Join(report.Aggregate.RelayModes, ","))
	}
	for _, run := range report.Runs {
		status := "ok"
		if run.Error != "" {
			status = "error=" + run.Error
		}
		fmt.Printf("run[%d]: wall-ms=%d relay-mode=%s batch-count=%d planned-batches=%d target=%s %s\n",
			run.Index, run.WallMillis, run.Result.RelayMode, run.Result.BatchCount, run.Result.PlannedBatchCount, run.TargetPath, status)
	}
}

type multiStringFlag []string

func (m *multiStringFlag) String() string {
	return strings.Join(*m, ",")
}

func (m *multiStringFlag) Set(value string) error {
	*m = append(*m, value)
	return nil
}

func splitCSV(value string) []string {
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func usageError(message string) error {
	usage := "usage:\n  git-sync-bench --source-url <repo> [flags]\n\nflags:\n  --scenario bootstrap|sync\n  --repeat 3\n  --work-dir /tmp/git-sync-bench\n  --keep-targets\n  --json\n  --branch main,release\n  --map main:stable\n  --tags\n  --force\n  --prune\n  --stats\n  --measure-memory\n  --max-pack-bytes 104857600\n  --target-max-pack-bytes 104857600\n  --protocol auto|v1|v2\n  -v\n"
	if message == "" {
		return errors.New(strings.TrimSpace(usage))
	}
	return fmt.Errorf("%s\n\n%s", message, usage)
}

type benchProtocolMode gitsync.ProtocolMode

type benchProtocolModeFlag benchProtocolMode

func (p *benchProtocolModeFlag) String() string {
	return string(*p)
}

func (p *benchProtocolModeFlag) Set(value string) error {
	mode, err := validation.NormalizeProtocolMode(value)
	if err != nil {
		return fmt.Errorf("normalize protocol: %w", err)
	}
	*p = benchProtocolModeFlag(benchProtocolMode(gitsync.ProtocolMode(mode)))
	return nil
}
