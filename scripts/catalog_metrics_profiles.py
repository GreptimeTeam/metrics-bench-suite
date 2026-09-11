#!/usr/bin/env python3
"""Inventory original configs without rewriting them or expanding their series."""
import argparse
import collections
import json
from pathlib import Path
import subprocess

ROOT = Path(__file__).resolve().parents[1]
CURATED = {"debug_samples_20": "k8s-small", "debug_samples_400": "k8s-medium", "samples_1750": "k8s-large"}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--generator", type=Path, default=ROOT / "bin/metrics_dataset")
    parser.add_argument("--output", type=Path)
    args = parser.parse_args()
    catalog = []
    for directory in sorted((ROOT / "configs").iterdir()):
        if not directory.is_dir():
            continue
        result = subprocess.run([str(args.generator), "inspect", "--config", str(directory)], text=True, capture_output=True)
        if not result.stdout:
            raise SystemExit(result.stderr)
        inspection = json.loads(result.stdout)
        metrics = inspection["metrics"]
        families = collections.Counter(metric["name"].split("_")[0] for metric in metrics)
        distributions = collections.Counter(metric["value_distribution"] for metric in metrics)
        labels = sorted({label for metric in metrics for label in metric["label_cardinality"]})
        catalog.append({
            "source": f"configs/{directory.name}",
            "curated_copy": f"profiles/{CURATED[directory.name]}" if directory.name in CURATED else None,
            "category": "curated-source" if directory.name in CURATED else "source-collection" if directory.name in ("metrics", "1000k_seires_sample") else "legacy-scale-variant",
            "valid": inspection["valid"],
            "base_series": inspection["base_series"] if inspection["valid"] else None,
            "validated_metric_count": len(metrics),
            "metric_families": dict(sorted(families.items())),
            "label_dimensions": labels,
            "value_distributions": dict(distributions),
            "config_sha256": inspection["config_sha256"],
            "errors": [error.replace(str(ROOT) + "/", "") for error in inspection["errors"]],
        })
    text = json.dumps({"schema_version": 1, "collections": catalog}, indent=2) + "\n"
    if args.output:
        args.output.write_text(text)
    else:
        print(text, end="")


if __name__ == "__main__":
    main()
