# Metrics Loader Helm Chart

This chart runs `sample_loader` as an Indexed Kubernetes Job. Each Job completion can use its completion index as the `replica` label so parallel writers generate distinct series.

## Install

Prometheus remote write:

```bash
helm upgrade --install metrics-loader ./tools/metrics-loader \
  --set-string sampleLoader.remoteWriteUrl='http://greptimedb:4000/v1/prometheus/write?db=public'
```

OTLP Metrics over HTTP/protobuf:

```bash
helm upgrade --install metrics-loader ./tools/metrics-loader \
  --set sampleLoader.protocol=otlp \
  --set-string sampleLoader.remoteWriteUrl='http://otel-collector:4318/v1/metrics'
```

Use `sampleLoader.infinite=false` with `startDate` and `endDate` for a finite historical run. To generate from the current time for a bounded duration:

```bash
helm upgrade --install metrics-loader ./tools/metrics-loader \
  --set sampleLoader.infinite=false \
  --set-string sampleLoader.duration=60s \
  --set-string sampleLoader.remoteWriteUrl='http://greptimedb:4000/v1/prometheus/write?db=public'
```

The chart accepts positive whole-unit durations using `ns`, `us`, `ms`, `s`, `m`, or `h`, with up to six digits. The duration timer starts after the loader's startup jitter.

For Basic Auth, direct `sampleLoader.username` and `sampleLoader.password` values are supported but are visible in Helm release data and the Pod manifest. In production, create a Secret with `username` and `password` keys and set `basicAuth.existingSecret` instead. The key names can be changed with `basicAuth.usernameKey` and `basicAuth.passwordKey`.

The image contains sample configurations under `/configs`.

The Job name includes the Helm release revision. `helm upgrade` therefore replaces the Job when image, arguments, or Pod settings change instead of attempting to update its immutable Pod template.

## Sample Loader Values

| Value | CLI argument | Default |
| --- | --- | --- |
| `sampleLoader.config` | `--config` | `/configs/debug_samples` |
| `sampleLoader.remoteWriteUrl` | `--remote-write-url` | Required unless dry-run |
| `sampleLoader.remoteWriteVersion` | `--remote-write-version` | `v1` |
| `sampleLoader.username` | `--username` | Empty |
| `sampleLoader.password` | `--password` | Empty |
| `sampleLoader.startDate` | `--start-date` | `2025-01-01T00:00:00Z` |
| `sampleLoader.endDate` | `--end-date` | `2025-01-01T00:01:00Z` |
| `sampleLoader.interval` | `--interval` | `30s` |
| `sampleLoader.maxSamples` | `--max-samples` | `20000` |
| `sampleLoader.tickInterval` | `--tick-interval` | `30s` |
| `sampleLoader.workers` | `--workers` | `1` |
| `sampleLoader.replica` | `--replica` | `0` |
| `sampleLoader.infinite` | `--infinite` | `true` |
| `sampleLoader.duration` | `--duration` | Empty |
| `sampleLoader.tablePickCount` | `--table-pick-count` | `18446744073709551615` |
| `sampleLoader.dryRun` | `--dry-run` | `false` |
| `sampleLoader.churnRate` | `--churn-rate` | `0` |
| `sampleLoader.churnInterval` | `--churn-interval` | `0s` |
| `sampleLoader.protocol` | `--protocol` | `prometheus` |

Set `sampleLoader.replicaFromJobCompletionIndex=false` to pass `sampleLoader.replica` directly. When enabled, the Job completion index overrides that value.
