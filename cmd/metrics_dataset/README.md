# Historical metrics datasets

`metrics_dataset` creates reusable Prometheus remote-write v1 files without a
server. Build with Go 1.23 or newer from the repository root:

```bash
go build -o bin/metrics_dataset ./cmd/metrics_dataset
./bin/metrics_dataset inspect --profile k8s-small
./bin/metrics_dataset generate --profile k8s-small \
  --output-dir /tmp/metrics-k8s-small
./bin/metrics_dataset verify --data-dir /tmp/metrics-k8s-small
```

Use `--profiles-dir /path/to/metrics-bench-suite/profiles` when running from a
different working directory. `--config /path/to/configs` accepts custom YAML
instead of a curated profile. Existing `sample_generator` and `sample_loader`
commands retain their defaults, Parquet/HTTP output, and live timing behavior.

## Generation controls

| Flag | Default | Meaning |
| --- | --- | --- |
| `--profile` | required unless `--config` | `k8s-small`, `k8s-medium`, or `k8s-large` |
| `--start-date` | `2025-03-21T09:45:00Z` | Inclusive RFC3339 start |
| `--end-date` | `2025-03-21T10:21:00Z` | Exclusive RFC3339 end |
| `--interval` | `30s` | Stored sample interval, in whole milliseconds |
| `--seed` | `123456` | Unsigned 64-bit random seed |
| `--replica` | `0` | Added replica label, nonnegative |
| `--churn-rate` | `0` | Fraction of base series selected for churn |
| `--churn-interval` | `0s` | Dataset-time epoch length; must be positive when churn is enabled |
| `--max-samples-per-request` | `10000` | Sample cap per output file |
| `--output-dir` | required | New or empty output directory |

CLI timestamps override any embedded `start`, `end`, or `interval` in the YAML.
Every series has a sample at `start + n * interval` while that timestamp is less
than `end`. Intervals need not divide the window evenly.

```bash
./bin/metrics_dataset generate --profile k8s-small \
  --seed 42 --churn-rate 0.1 --churn-interval 10m \
  --output-dir /tmp/metrics-k8s-small-churn
```

Churn uses the same deterministic per-metric prefix selection as the live loader.
Selected series gain `churn_id=epoch_N`, with N computed from elapsed dataset
time. Field values continue across epochs. Epochs skipped between scrapes do not
create series. No stale markers are emitted: queries may see old identities
within their lookback window after a churn event.

Random distributions use private per-base-series `golang.org/x/exp/rand` streams.
The seed is the first eight SHA-256 bytes, interpreted little-endian, of the Go
JSON encoding of `{Seed, Labels}` with canonical sorted base labels including the
metric and replica. Batch sizes do not change logical values. Fixed binary,
configs, and options produce identical files regardless of wall-clock speed.
The existing distributions are preserved, including their current synthetic
counter/gauge behavior.

## File contract, version 1

Each `prw-000000000000.bin` file contains one protobuf `prompb.WriteRequest`
compressed using Snappy block encoding. Load files in manifest order. Each base
series is emitted in timestamp order; global timestamps restart for each base
series. Long series and churn epochs can span files. The format contains scalar
samples only, with no OTLP, remote-write v2, exemplars, or native histograms.

`summary.json` contains:

- schema/format versions, generator revision, dirty-build flag, and executable SHA-256;
- resolved options, configuration digest, per-file config hashes, metrics, and label cardinalities;
- base-series count, distinct emitted identities including churn, actual samples;
- ordered output filenames, sample counts, byte sizes, and SHA-256 hashes;
- dataset identity and generation duration, with duration excluded from identity.

The dataset identity hashes the canonical Go JSON summary after clearing
`dataset_id` and zeroing generation duration. It includes the binary identity and
batch size: two different artifacts can contain equivalent logical samples.
Absolute output/config locations are excluded. `version` reports executable
identity for callers such as O11yBench.

`verify` checks manifest identity, all file checksums, decoded counts, finite
values, ordered labels, replica identity, and complete per-base-series timestamp
sequences. It rejects missing/extra files and unfinished datasets. A summary is
published only after all output files finish; failed/interrupted outputs are
left for diagnosis and cannot be reused as complete datasets.

Generation retains one series' field state and one bounded batch, plus config
label candidates and output-file metadata. It does not allocate the complete
series product or a generator for every series. `inspect` computes cardinality
without expanding label candidates.

## Validation

```bash
go test ./pkg/dataset ./pkg/samples ./pkg/cmd/sample_generator ./pkg/cmd/sample_loader
python3 scripts/catalog_metrics_profiles.py --output /tmp/metrics-catalog.json
```

Dataset tests independently decode the wire files and exercise all distributions,
reproducibility, batch changes, resets, historical churn, cancellation, and
corrupt/incomplete output. See [the profile catalog](../../profiles/README.md)
for exact copies and known invalid legacy collections.
