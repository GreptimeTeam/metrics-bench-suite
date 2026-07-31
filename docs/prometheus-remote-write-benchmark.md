# Benchmark Prometheus Remote Write

Use `sample_loader` to generate sustained synthetic Prometheus remote-write
traffic in v1 or v2 format. It creates label sets from the bundled YAML files,
generates one sample per series in each wave, splits the wave into requests, and
sends those requests with configurable concurrency.

## Prerequisites

- Go 1.23 or newer.
- A receiver that accepts the selected Prometheus remote-write message.
- Enough client and receiver memory for the selected series cardinality.

Build only the required binary:

```bash
make sample_loader
```

The resulting executable is `./bin/sample_loader`.

## Choose the remote-write version

Remote write v1 remains the default. Select remote write 2.0 explicitly:

```bash
--remote-write-version v2
```

The accepted values are `v1` and `v2`. The v2 mode sends
`io.prometheus.write.v2.Request` with symbolized label references. Both modes
send the same generated float samples and use Snappy block compression.

## Verify the workload first

Always run a single-wave dry run before sending data:

```bash
./bin/sample_loader \
  --dry-run \
  --config ./configs/debug_samples_20 \
  --remote-write-version v2 \
  --start-date 2025-01-01T00:00:00Z \
  --end-date 2025-01-01T00:00:00Z \
  --tick-interval 1ms \
  --max-samples 5000
```

For the current `debug_samples_20` profile, this prints:

```text
Total series: 20601
```

It then reports four batches of 5,000 series and one batch of 601 series.
Dry-run mode does not require `--remote-write-url` and sends no network traffic.

## Run a sustained benchmark

### GreptimeDB

The following command sends approximately 20,601 samples per second to a local
GreptimeDB instance:

```bash
./bin/sample_loader \
  --config ./configs/debug_samples_20 \
  --remote-write-url 'http://localhost:4000/v1/prometheus/write?db=public' \
  --remote-write-version v2 \
  --duration 60s \
  --interval 1s \
  --tick-interval 1s \
  --max-samples 5000 \
  --workers 4 \
  --replica 0
```

This generates live samples for 60 seconds, then stops generating the current
wave and drains queued requests for up to 30 seconds before canceling
unfinished requests. Use `--infinite` instead to run until interrupted with
`Ctrl-C`; the two flags cannot be combined.

For HTTP Basic authentication, add both flags:

```bash
--username myuser --password mypassword
```

### Prometheus

Prometheus must be started with:

```text
--web.enable-remote-write-receiver
--web.remote-write-receiver.accepted-protobuf-messages=io.prometheus.write.v2.Request
```

Then use this receiver URL:

```text
http://localhost:9090/api/v1/write
```

Prometheus documents this receiver as a low-volume facility rather than an
efficient replacement for scraping. Keep that limitation in mind when
interpreting a Prometheus-server ingestion benchmark. See the
[Prometheus remote-write receiver documentation][prometheus-receiver].

For another database, replace `--remote-write-url` with that product's
Prometheus remote-write endpoint.

## Understand the load controls

`sample_loader` has two independent time controls:

- `--interval` advances the timestamp stored in each generated sample.
- `--tick-interval` controls the real wall-clock delay between generated waves.

For realistic live ingestion, keep them equal. To load historical data faster
than real time, use a larger `--interval` than `--tick-interval`.

The main sizing flags are:

| Flag | Meaning |
| --- | --- |
| `--config` | One YAML file or a directory of YAML files. |
| `--remote-write-version` | Wire message to send: `v1` by default, or `v2`. |
| `--max-samples` | Maximum time series per request. Each generated series contains one sample. |
| `--workers` | Number of concurrent request workers. |
| `--replica` | Value of the `replica` label injected into every series. |
| `--infinite` | Start at the current time and run until interrupted. |
| `--duration` | Start at the current time and generate waves for a finite wall-clock duration, such as `60s`. |
| `--start-date`, `--end-date` | Inclusive logical timestamp range for a finite run. |
| `--churn-rate` | Fraction of series whose identity changes at each churn epoch. |
| `--churn-interval` | Wall-clock duration between churn epochs. |

Given:

- `S` series,
- batch size `B`, and
- tick interval `T` seconds,

the configured load is:

```text
requests per wave = ceil(S / B)
offered samples/second = S / T
```

For a finite run:

```text
waves = floor((end - start) / interval) + 1
total samples = S * waves
```

The request channel applies backpressure. If the receiver cannot keep up, actual
throughput falls below the configured offered rate instead of growing an
unbounded request queue. The duration deadline also stops an in-progress wave;
queued requests then drain for up to 30 seconds before cancellation.

## Bundled workload sizes

Profile names are approximate. These are the cardinalities calculated from the
current YAML files:

| Config directory | Series |
| --- | ---: |
| `configs/debug_samples_20` | 20,601 |
| `configs/debug_samples_200` | 234,566 |
| `configs/debug_samples_400` | 416,370 |
| `configs/debug_samples_800` | 809,664 |
| `configs/debug_samples_1200` | 1,132,518 |
| `configs/debug_samples_1200_v2` | 1,258,566 |
| `configs/samples_1750` | 1,755,410 |
| `configs/debug_samples` | 31,813,594 |

The bare `debug_samples` directory is much larger than its older README
description. Do not run it without checking available memory and performing a
dry run.

Avoid `--table-pick-count` when exact table count matters. The current
implementation includes one more YAML file than the requested count. Use a
directory containing exactly the desired YAML files instead.

## Create a custom workload

Each YAML filename becomes the metric name. For example,
`http_requests_total.yaml` creates `http_requests_total`.

```yaml
tags:
  - name: instance
    type: String
    dist:
      type: replica_string
      replica: 1000
      replica_prefix: host-
fields:
  - name: greptime_value
    type: Float
    dist:
      type: mono_inc
      lower_bound: 0
      upper_bound: 1000000
      step: 1
```

This file produces 1,000 series before the loader adds its own `replica` label.
With multiple label definitions, series cardinality is the Cartesian product of
their candidate counts. Large candidate sets can therefore consume substantial
memory.

The loader uses the first field definition. Timestamp ranges and cadence come
from the command-line flags, not the `start`, `end`, or `interval` fields that
may exist in older YAML files. The label name `replica` is reserved for the
loader.

## Run multiple loaders and model churn

Give every loader process or pod a different `--replica` value so that they do
not write identical label sets:

```bash
--replica 0
--replica 1
--replica 2
```

To change 1% of series identities every ten wall-clock minutes:

```bash
--churn-rate 0.01 --churn-interval 10m
```

Churn adds a changing `churn_id` label to the selected series. The epoch is based
on elapsed wall-clock time, not generated sample timestamps.

## Measure the benchmark

The loader logs request duration, worker number, and series count:

```text
worker 0 sent request in 42ms, num series: 5000
```

After a duration run stops generation and finishes its bounded drain, it logs
client-side totals and throughput:

```text
Run statistics: elapsed=1m0.042s requests_total=250 requests_succeeded=249 requests_failed=1 samples_total=1230100 samples_in_succeeded_requests=1225100 samples_in_failed_requests=5000 samples_per_second=20403.98 dry_run=false
```

The rate counts samples from requests the client considered successful and
divides by total elapsed time, including queue draining. Samples in a failed v2
request may have been partially accepted by the receiver, so use receiver
metrics for authoritative accepted counts. The loader does not calculate
latency percentiles or resource usage. Measure those on the receiver:

- accepted samples or rows per second,
- request errors and rejected samples,
- CPU and memory,
- disk and network throughput,
- compaction or background-work pressure,
- persisted row or sample count after the run.

Also search the loader output for `failed to send write request`. Worker errors
are logged but are not propagated to the process exit status.

For v2, the loader also requires
`X-Prometheus-Remote-Write-Samples-Written` in every successful response and
checks that it matches the number sent. A missing, malformed, or partial count
is logged as a failed request.

Keep the workload profile, batch size, worker count, tick interval, receiver
configuration, warm-up duration, and measurement window fixed when comparing
runs.

## Wire formats and limitations

The `sample_loader` sender supports:

- v1 `prometheus.WriteRequest`;
- v2 `io.prometheus.write.v2.Request` with a deduplicated symbol table and
  `labels_refs`;
- uses Snappy block compression;
- sends the message-specific `Content-Type`;
- sends `Content-Encoding: snappy`;
- sends `X-Prometheus-Remote-Write-Version: 0.1.0` for v1 or `2.0.0` for v2;
- sends `User-Agent: metrics-bench-suite`;
- generates float samples only.

V2 mode does not currently generate native histograms, exemplars, metadata, or
created timestamps. The `_v2` suffix in a config directory name does not select
the wire protocol; only `--remote-write-version` does. See the
[Prometheus remote-write specification][prometheus-spec] for the full protocol.
The optional text dump represents the decoded protobuf message, not the exact
Snappy-compressed bytes sent over HTTP.

The relevant implementation is in:

- [`pkg/cmd/sample_loader/sample_loader.go`](../pkg/cmd/sample_loader/sample_loader.go)
- [`pkg/samples/file_config_timeseries.go`](../pkg/samples/file_config_timeseries.go)
- [`pkg/http/requester.go`](../pkg/http/requester.go)

## Other tools

- `loader` extracts real label sets from a tcpflow-reassembled remote-write
  capture, generates new sample values, and replays them continuously. Use it
  when realistic production label shapes matter more than a controlled
  synthetic profile. It currently sends v1 only.
- `sample_generator` builds a complete historical range into one write request.
  It is useful for small data generation jobs, but not for sustained or
  high-cardinality ingestion benchmarks. It currently sends v1 only.
- `timeseries_analyzer` counts metrics and series in a captured remote-write
  request stream.
- `remote_write_request_viewer` decodes and displays a base64-encoded,
  Snappy-compressed remote-write v1 body.

The command-specific READMEs under `cmd/` contain examples for these secondary
tools.

[prometheus-receiver]: https://prometheus.io/docs/prometheus/latest/querying/api/#remote-write-receiver
[prometheus-spec]: https://prometheus.io/docs/specs/prw/remote_write_spec/
