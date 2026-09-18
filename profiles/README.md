# Curated metrics profiles

These profiles derive from selected existing config sets, correcting scalar
value behavior and the label relationships required by the O11yBench Kubernetes
dashboard workload. The revised profiles have 109 metrics.
All original files under `configs/` remain unchanged.

| Profile | Derived from | Metrics | Base series | Samples in default 36m / 30s window |
| --- | --- | ---: | ---: | ---: |
| `k8s-small` | `configs/debug_samples_20` | 109 | 20,205 | 1,454,760 |
| `k8s-medium` | `configs/debug_samples_400` | 109 | 406,610 | 29,275,920 |
| `k8s-large` | `configs/samples_1750` | 109 | 1,747,610 | 125,827,920 |

The profiles cover container, kubelet, kube-state, coredns, node, and supporting
metrics. They expand label combinations across dimensions such as namespace,
pod, node, instance, and environment. Directory suffixes are not authoritative
series counts. These three profiles have no duplicate metric names or candidate
values and pass strict dataset validation.

## Dashboard compatibility revision

The profiles now include `node_namespace_pod_container:container_cpu_usage_seconds_total:sum_rate5m`,
with the same dimensions/value distribution as the retained `sum_irate` metric.
These are synthetic stored recording-rule outputs, not evaluated recording rules.
Owner records use one `workload-0` / `deployment` owner per namespace/pod;
`kube_pod_info` uses `host_network="false"`. cAdvisor metrics retain
`job="kubelet",metrics_path="/metrics/cadvisor"`; use that mixin selector override.
Throttled and total period counters have identical label domains and increments
of 1 and 10, yielding a 0.1 throttling ratio. Namespace CPU request sums use
[1,1000), preventing zero denominators. All three profiles enforce these changes.
Regenerate old datasets: counts, config hashes and dataset identities changed.

Histogram buckets still have identical increments, including +Inf: this is a
valid degenerate distribution with all observations in the lowest bucket.
The selected PLEG histogram has first boundary 0.005 seconds, so p99 is 0.00495.
It does not model realistic latency distributions. Other legacy resource and
cross-metric relationships remain uncorrelated.

## Scalar value behavior

The three sizes use identical value definitions for each metric:

| Category | Distribution | Values |
| --- | --- | --- |
| Raw counters and histogram buckets/counts | `mono_inc` | Start at 0, add 10 per sample (throttled periods: 1), no upper-bound reset |
| Info, ownership/relabel, node-name, synthetic `container`, and healthy `up` | `constant_float` | 1.0 |
| `kubelet_node_config_error` | `constant_float` | 0.0 |
| State, condition, phase, and termination reason | `random_int` | 0 or 1 |
| Availability and CPU ratio recording rules | `uniform` | [0, 1) |
| CPU rate recording rule | `uniform` | [0, 16) cores |
| Request rate recording rule | `uniform` | [0, 1000) requests/second |
| Latency quantile recording rule | `uniform` | [0, 5) seconds |
| Memory/storage gauges and their recording rules | `uniform` | [0, 1073741824) bytes |
| Running pods/containers, goroutines, cache entries/size, queue depth, volume counts | `random_int` | Integers from 0 through 1000 |
| Remaining resource/request/limit/quota gauges | `uniform` | [0, 1000); namespace CPU requests: [1, 1000) |

`[a, b)` includes a and excludes b. Raw `node_netstat_*` values are counters;
`kubelet_running_pod_count` and `kubelet_running_container_count` are gauges.
Recording rules are classified by their result, even when their names contain
`_total`. These choices follow [Prometheus metric semantics](https://prometheus.io/docs/concepts/metric_types/).

These are synthetic scalar workloads. Histograms remain degenerate, statuses are
not mutually exclusive, and resource/unit labels and most cross-metric
relationships are not correlated. The dashboard-specific corrections above are
the explicit exceptions. Capacity gauges also vary in this model. The bare `container` metric is treated as a synthetic presence value.
Differently named legacy aliases remain distinct metrics. No duplicate metric
names or label candidates were found in the curated profiles.

Weighted presets enumerate every candidate; weights do not affect frequency when
enumerating the Cartesian product. Empty label values are omitted, as in the live
loader. Corrected values change config/dataset digests and may change compression
results compared with the original all-counter workloads.

## Inventory of original collections

The machine-readable [catalog](catalog.json) records each original directory's
category, validation status, metric families, label dimensions, value
distributions, config digest, and file-specific errors. An invalid collection's
base-series total is null rather than presenting its valid subset as complete.
The catalog describes original collections, including their original value
distributions and digests. Its `curated_copy` field records source lineage, not
byte-for-byte equality with the corrected profiles.

| Original directory | Strict dataset status | Base series if valid |
| --- | --- | ---: |
| `1000k_seires_sample` | Duplicate metric names; source collection | — |
| `debug_samples` | Unsupported `precision` keys in three configs | — |
| `debug_samples_0` | Valid legacy micro profile | 8 |
| `debug_samples_20` | Curated source | 20,601 |
| `debug_samples_200` | Valid legacy scale variant | 234,566 |
| `debug_samples_400` | Curated source | 416,370 |
| `debug_samples_800` | Valid legacy scale variant | 809,664 |
| `debug_samples_1200` | Valid legacy scale variant | 1,132,518 |
| `debug_samples_1200_v2` | Valid metric-set variant | 1,258,566 |
| `debug_samples_1500` | Valid legacy scale variant | 1,022,948 |
| `metrics` | Source collection with unsupported keys/schema errors | — |
| `samples_1750` | Curated source | 1,755,410 |

Only the three curated directories have named dataset profiles. Other valid
collections can be supplied explicitly with `--config`; invalid ones fail with
diagnostics. `_v2` denotes a config variant, not a wire-protocol version.

Regenerate the inventory without editing any original config:

```bash
go build -o bin/metrics_dataset ./cmd/metrics_dataset
python3 scripts/catalog_metrics_profiles.py --output profiles/catalog.json
```

Use [metrics_dataset](../cmd/metrics_dataset/README.md) to inspect, generate, and
verify datasets. O11yBench consumes its executable and versioned file contract.
