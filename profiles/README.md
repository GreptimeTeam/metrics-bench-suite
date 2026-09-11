# Curated metrics profiles

These directories are **byte-for-byte copies** of selected existing config sets.
All original files under `configs/` remain unchanged. The copies preserve the
existing value distributions; realistic gauge/counter profiles and corrections
are separate future work.

| Profile | Copied from | Metrics | Base series | Samples in default 36m / 30s window |
| --- | --- | ---: | ---: | ---: |
| `k8s-small` | `configs/debug_samples_20` | 108 | 20,601 | 1,483,272 |
| `k8s-medium` | `configs/debug_samples_400` | 108 | 416,370 | 29,978,640 |
| `k8s-large` | `configs/samples_1750` | 108 | 1,755,410 | 126,389,520 |

The profiles cover container, kubelet, kube-state, coredns, node, and supporting
metrics. They expand label combinations across dimensions such as namespace,
pod, node, instance, and environment. Directory suffixes are not authoritative
series counts. These three copies have no duplicate metric names or candidate
values and pass strict dataset validation.

All bundled values use `mono_inc`, even for metric names normally representing
gauges or boolean availability. These are synthetic workload shapes, not a
claim of production value realism. Weighted presets enumerate every candidate;
weights do not affect frequency when enumerating the Cartesian product. Empty
label values are omitted, as in the live loader.

## Inventory of original collections

The machine-readable [catalog](catalog.json) records each original directory's
category, validation status, metric families, label dimensions, value
distributions, config digest, and file-specific errors. An invalid collection's
base-series total is null rather than presenting its valid subset as complete.

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

Only the three curated copies have named dataset profiles. Other valid
collections can be supplied explicitly with `--config`; invalid ones fail with
diagnostics. `_v2` denotes a config variant, not a wire-protocol version.

Regenerate the inventory without editing any original config:

```bash
go build -o bin/metrics_dataset ./cmd/metrics_dataset
python3 scripts/catalog_metrics_profiles.py --output profiles/catalog.json
```

Use [metrics_dataset](../cmd/metrics_dataset/README.md) to inspect, generate, and
verify datasets. O11yBench consumes its executable and versioned file contract.
