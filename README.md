# Metrics Bench Suite

Metrics Bench Suite is a set of tools designed to benchmark the storage and querying of metrics data in time-series databases

## Tools

- `timeseries_analyzer`: Analyze number of timeseries in tcpdump output.
- `remote_write_request_viewer`: View remote write requests details.
- `loader`: Load time series data into the target database using Prometheus remote write or OTLP Metrics over HTTP/protobuf.

The writing tools accept `--protocol prometheus` (the default) or `--protocol otlp`. Generated scalar time series are encoded as OTLP Gauge metrics; `__name__` becomes the metric name, other labels become string attributes, and timestamps are converted from milliseconds to nanoseconds.

## Helm Chart

Deploy `sample_loader` as an Indexed Kubernetes Job. Each Job completion uses its completion index as the replica label, so parallel writers generate distinct series.

Prometheus remote write:

```bash
helm upgrade --install metrics-loader ./tools/metrics-loader \
  --set replicaCount=3 \
  --set-string sampleLoader.remoteWriteUrl='http://greptimedb:4000/v1/prometheus/write?db=public'
```

OTLP Metrics over HTTP/protobuf:

```bash
helm upgrade --install metrics-loader ./tools/metrics-loader \
  --set replicaCount=3 \
  --set sampleLoader.protocol=otlp \
  --set-string sampleLoader.remoteWriteUrl='http://otel-collector:4318/v1/metrics'
```

Run the workload without sending samples:

```bash
helm upgrade --install metrics-loader ./tools/metrics-loader \
  --set sampleLoader.dryRun=true
```

Set `sampleLoader.infinite=false` with `sampleLoader.startDate` and `sampleLoader.endDate` for a finite run. See [`tools/metrics-loader/README.md`](tools/metrics-loader/README.md) for all values and their corresponding `sample_loader` arguments.
