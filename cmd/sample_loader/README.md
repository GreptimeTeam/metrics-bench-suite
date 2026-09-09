### Sample Loader

The sample loader generates samples and sends them using Prometheus remote write or OTLP Metrics over HTTP/protobuf.

#### Usage

Generate and load sample data with Prometheus remote write v1 (the default):

```bash
./bin/sample_loader -c ./configs/debug_samples_400 -u 'http://localhost:4000/v1/prometheus/write?db=public' --start-date 2025-03-09T18:00:00+08:00 --end-date 2025-03-09T19:00:00+08:00 --interval 30s --tick-interval 1s
```

To send `io.prometheus.write.v2.Request`:

```bash
./bin/sample_loader -c ./configs/debug_samples_400 -u 'http://localhost:4000/v1/prometheus/write?db=public' --remote-write-version v2
```

Send OTLP Metrics to an OpenTelemetry Collector HTTP receiver:

```bash
./bin/sample_loader -c ./configs/debug_samples_400 -u http://localhost:4318/v1/metrics --protocol otlp --start-date 2025-03-09T18:00:00+08:00 --end-date 2025-03-09T19:00:00+08:00 --interval 30s --tick-interval 1s
```

`--protocol` accepts `prometheus` (default) or `otlp`. `--remote-write-version` applies only to Prometheus.
The `-u, --remote-write-url` flag is the destination endpoint for either protocol.

Use `--duration 60s` to generate live samples for a finite wall-clock duration.
It cannot be combined with `--infinite`. Generation stops at the deadline;
queued requests then drain for up to 30 seconds before unfinished requests are
canceled. The loader prints request and sample totals, failures, and samples
in successful requests per second.

HTTP Basic authorization is available for either protocol:

```bash
./bin/sample_loader -c ./configs/debug_samples_400 -u 'http://localhost:4000/v1/prometheus/write?db=public' --username myuser --password mypassword
```

#### Progress output

Instead of logging every successful request, the loader prints one aggregate progress line per second:

```text
time=2026-09-09T12:00:01Z rows_written=120000 churn_epoch=3 avg_latency=12.4ms p99_latency=21.7ms throughput=60000.00_rows/s avg_throughput=58500.00_rows/s
```

- `rows_written` counts accepted data points, including accepted points from OTLP partial-success responses.
- `avg_latency`, `p99_latency`, and `throughput` cover the latest reporting window.
- `avg_throughput` covers the complete run since sending started.
- `churn_epoch` is included only when both `--churn-rate` and `--churn-interval` enable churn.
- Fully rejected requests are logged separately and are not included in row or throughput totals.

#### Configs

`./configs/debug_samples` total time series 31,813,594.
`./configs/debug_samples_400` total time series 416,370.
`./configs/debug_samples_800` total time series 809,664.
