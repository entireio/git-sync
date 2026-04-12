# Benchmarking

`git-sync-bench` runs repeatable empty-target benchmarks against a source repository.

It currently supports:

- `bootstrap`: calls `syncer.Bootstrap` directly.
- `sync`: calls `syncer.Run`, which may choose bootstrap relay automatically on an empty target.

The tool creates a fresh bare target repository for each run and reports both wall-clock time and the internal `syncer` measurement data.

## Build

```bash
go build -o /tmp/git-sync-bench ./cmd/git-sync-bench
```

## Example

Against a local mirror:

```bash
/tmp/git-sync-bench \
  --scenario bootstrap \
  --source-url /tmp/git-sync-bench/kubernetes.git \
  --repeat 3 \
  --batch-max-pack-bytes 104857600 \
  --stats \
  --json
```

If `--source-url` is a filesystem path, the tool converts it to `file://...` automatically.

## Output

The JSON report includes:

- per-run wall time
- per-run `syncer.Result`
- aggregate min/avg/max wall time
- aggregate min/avg/max internal elapsed time
- maximum observed alloc and heap-inuse peaks
- relay modes seen across successful runs

## Notes

- `bootstrap` benchmarks reject `--force` and `--prune`, matching `git-sync bootstrap`.
- `--keep-targets` retains the generated bare targets under `--work-dir` for inspection.
- For large real-repo runs, prefer using a local mirror rather than benchmarking directly against a hosted remote.
