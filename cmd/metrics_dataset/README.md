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
compressed using Snappy block encoding. Load files in manifest order. New datasets
emit **all series at one timestamp before advancing to the next timestamp**, in
stable metric-name and label-permutation order, simulating successive scrapes.
Each protobuf time-series entry contains one sample; the same labels can appear
again later in a request at a subsequent timestamp. Requests fill to
`--max-samples-per-request`, so a scrape can span files and a file can contain
multiple scrapes. Only the final request may be partially filled. Generation
uses historical timestamps without waiting for wall-clock scrape intervals.

The format contains scalar samples only, with no OTLP, remote-write v2,
exemplars, or native histograms.

`summary.json` contains:

- schema/format versions, generator revision, dirty-build flag, and executable SHA-256;
- `sample_order: "timestamp-major"` for newly generated datasets;
- resolved options, configuration digest, per-file config hashes, metrics, and label cardinalities;
- base-series count, distinct emitted identities including churn, actual samples;
- ordered output filenames, sample counts, byte sizes, and SHA-256 hashes;
- dataset identity and generation duration, with duration excluded from identity.

The dataset identity hashes the canonical Go JSON summary after clearing
`dataset_id` and zeroing generation duration. It includes the binary identity and
batch size: two different artifacts can contain equivalent logical samples.
The optional `sample_order` field is part of the identity. An absent field means
legacy series-first output, where global timestamps restart for each base series.
`verify` still accepts those datasets with their original identities; generation
only produces timestamp-first output. Unknown ordering values are rejected.
Schema and CLI contract versions remain 1, and the remote-write encoding is
unchanged. Older verifiers cannot verify the new manifests: their identity check
rejects the additional ordering metadata. Use this version's verifier for new
outputs. Existing datasets need no conversion.

Absolute output/config locations are excluded. `version` reports executable
identity for callers such as O11yBench.

`verify` checks manifest identity, all file checksums, decoded counts, finite
values, ordered labels, replica identity, and complete timestamp sequences.
For timestamp-first output, it also checks exactly one sample per series entry,
complete scrapes, stable base-series order without duplicates, and the expected
churn selection and epochs. It rejects missing/extra files and unfinished datasets.
A summary is published only after all output files finish; failed/interrupted outputs are
left for diagnosis and cannot be reused as complete datasets.

Generation retains each base series' field/random state and one bounded batch,
plus config label candidates and output-file metadata. It regenerates label
combinations for each scrape rather than retaining the complete label product,
and never retains sample history. Memory therefore grows with series cardinality
and request size, plus the file inventory, rather than all generated samples.
This uses more field-state memory than legacy series-first generation while
preserving the same per-series random values and stateful distributions.
Verification retains one fingerprint per base series, with a uniqueness set
during the first scrape, instead of historical samples or churn identities.
`inspect` still computes cardinality without expanding label candidates.

Compressed size is not guaranteed to decrease: timestamp-first output repeats
labels per sample, but avoids making the distance between successive series'
samples depend on the total dataset duration.

## Validation

```bash
go test ./pkg/dataset ./pkg/samples ./pkg/cmd/sample_generator ./pkg/cmd/sample_loader
python3 scripts/catalog_metrics_profiles.py --output /tmp/metrics-catalog.json
```

Dataset tests independently decode the wire files and exercise all distributions,
reproducibility, batch changes across scrape boundaries, resets, historical churn,
cancellation, legacy compatibility, and corrupt/incomplete output. A tiny frozen
legacy fixture checks that logical samples remain unchanged across ordering modes;
semantic corruption tests rebuild integrity metadata before verification. See
[the profile catalog](../../profiles/README.md) for corrected curated values,
source lineage, and known invalid legacy collections.
