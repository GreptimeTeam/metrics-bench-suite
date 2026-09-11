package dataset_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/golang/snappy"
	"github.com/prometheus/prometheus/prompb"
	"metrics-bench-suite/pkg/dataset"
)

// Decode independently of Verify, retaining only these intentionally tiny fixtures.
func decode(t *testing.T, root string, limit int) map[string][]prompb.Sample {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(root, "*.bin"))
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(paths)
	result := map[string][]prompb.Sample{}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := snappy.Decode(nil, data)
		if err != nil {
			t.Fatal(err)
		}
		var request prompb.WriteRequest
		if err := request.Unmarshal(raw); err != nil {
			t.Fatal(err)
		}
		count := 0
		for _, series := range request.Timeseries {
			labels, _ := json.Marshal(series.Labels)
			key := string(labels)
			result[key] = append(result[key], series.Samples...)
			count += len(series.Samples)
			for i := 1; i < len(series.Labels); i++ {
				if series.Labels[i-1].Name >= series.Labels[i].Name {
					t.Fatalf("labels are not sorted/unique: %s", key)
				}
			}
		}
		if count == 0 || count > limit {
			t.Fatalf("request sample limit: %d", count)
		}
	}
	return result
}

func fixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	distributions := map[string]string{
		"counter":  "type: mono_inc\n      lower_bound: 1\n      upper_bound: 5\n      step: 2",
		"constant": "type: constant_float\n      value: 7",
		"normal":   "type: normal\n      mean: 10\n      stddev: 2",
		"random":   "type: random_float\n      lower_bound: 1\n      upper_bound: 5",
		"integer":  "type: random_int\n      lower_bound: 1\n      upper_bound: 5",
		"uniform":  "type: uniform\n      lower_bound: 1\n      upper_bound: 5",
		"noisy":    "type: noisy\n      max_fluctuation: 3",
		"periodic": "type: periodic\n      period: 10\n      amplitude: 2\n      bias: 1",
	}
	for name, dist := range distributions {
		config := fmt.Sprintf(`tags:
  - name: Zone
    type: STRING
    dist:
      type: constant_string
      value: zone-0
  - name: host
    type: STRING
    dist:
      type: replica_string
      replica: 2
      replica_prefix: host-
  - name: optional
    type: STRING
    dist:
      type: constant_string
      value: ''
fields:
  - name: greptime_value
    type: FLOAT
    dist:
      %s
`, dist)
		if err := os.WriteFile(filepath.Join(root, name+".yaml"), []byte(config), 0644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestDatasetRoundTrip(t *testing.T) {
	config := fixture(t)
	start := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, scenario := range []struct {
		name   string
		rate   float64
		churn  int64
		unique int64
	}{
		{"stable", 0, 0, 16}, {"partial", 0.5, 4000, 32}, {"full", 1, 4000, 48}, {"skipped_epochs", 1, 1000, 96},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			options := dataset.Options{Start: start, End: start.Add(11 * time.Second), IntervalMillis: 2000, Seed: 42, Replica: 7, ChurnRate: scenario.rate, ChurnIntervalMillis: scenario.churn, MaxSamples: 5}
			first, second := t.TempDir(), t.TempDir()
			s, err := dataset.Generate(context.Background(), config, first, options)
			if err != nil {
				t.Fatal(err)
			}
			repeat, err := dataset.Generate(context.Background(), config, second, options)
			if err != nil {
				t.Fatal(err)
			}
			if s.DatasetID != repeat.DatasetID || !reflect.DeepEqual(s.Files, repeat.Files) {
				t.Fatal("same inputs did not produce identical files")
			}
			verified, err := dataset.Verify(context.Background(), first)
			if err != nil {
				t.Fatal(err)
			}
			decoded := decode(t, first, 5)
			if verified.BaseSeries != 16 || verified.ActualSamples != 96 || verified.ActualSeries != scenario.unique || int64(len(decoded)) != scenario.unique {
				t.Fatalf("unexpected dataset counts: %+v, decoded=%d", verified, len(decoded))
			}
			count := 0
			for labels, points := range decoded {
				for _, point := range points {
					offset := point.Timestamp - start.UnixMilli()
					if offset < 0 || offset >= 11000 || offset%2000 != 0 {
						t.Fatalf("invalid timestamp %d", offset)
					}
					if strings.Contains(labels, `"value":"counter"`) && point.Value != []float64{1, 3, 5, 1, 3, 5}[offset/2000] {
						t.Fatalf("counter reset/state changed: %v", point)
					}
					if strings.Contains(labels, `"value":"constant"`) && point.Value != 7 {
						t.Fatalf("constant value: %v", point)
					}
					if strings.Contains(labels, `"optional"`) {
						t.Fatal("empty label was not omitted")
					}
					var pairs []prompb.Label
					if err := json.Unmarshal([]byte(labels), &pairs); err != nil {
						t.Fatal(err)
					}
					for _, pair := range pairs {
						if pair.Name == "churn_id" && pair.Value != fmt.Sprintf("epoch_%d", offset/scenario.churn) {
							t.Fatalf("churn did not follow sample time: %s", labels)
						}
					}
					count++
				}
			}
			if count != 96 {
				t.Fatalf("decoded %d samples", count)
			}
			// Batching must not change the generated values, timestamps, or identities.
			options.MaxSamples = 13
			optionsDir := t.TempDir()
			if _, err := dataset.Generate(context.Background(), config, optionsDir, options); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(decoded, decode(t, optionsDir, 13)) {
				t.Fatal("batch size changed logical samples")
			}
			options.Seed++
			different := t.TempDir()
			if _, err := dataset.Generate(context.Background(), config, different, options); err != nil {
				t.Fatal(err)
			}
			if reflect.DeepEqual(decoded, decode(t, different, 13)) {
				t.Fatal("seed did not affect random distributions")
			}
			// Integrity failures must not turn a damaged or incomplete dataset into a load.
			file := filepath.Join(first, s.Files[0].Name)
			data, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			data[0] ^= 1
			if err := os.WriteFile(file, data, 0644); err != nil {
				t.Fatal(err)
			}
			if _, err := dataset.Verify(context.Background(), first); err == nil {
				t.Fatal("corruption was accepted")
			}
			if err := os.Remove(filepath.Join(second, "summary.json")); err != nil {
				t.Fatal(err)
			}
			if _, err := dataset.Verify(context.Background(), second); err == nil {
				t.Fatal("incomplete output was accepted")
			}
		})
	}
	// An already canceled generation must not publish a complete dataset.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out := t.TempDir()
	_, err := dataset.Generate(ctx, config, out, dataset.Options{Start: start, End: start.Add(time.Second), IntervalMillis: 1000, MaxSamples: 10})
	if err == nil {
		t.Fatal("cancellation ignored")
	}
	if _, err := os.Stat(filepath.Join(out, "summary.json")); !os.IsNotExist(err) {
		t.Fatal("canceled generation published a summary")
	}
}

func TestCuratedProfilesAreExactValidCopies(t *testing.T) {
	for _, profile := range []struct {
		name, source string
		series       int64
	}{
		{"k8s-small", "debug_samples_20", 20601}, {"k8s-medium", "debug_samples_400", 416370}, {"k8s-large", "samples_1750", 1755410},
	} {
		source := filepath.Join("..", "..", "configs", profile.source)
		copyRoot := filepath.Join("..", "..", "profiles", profile.name)
		inspection, err := dataset.Inspect(copyRoot)
		if err != nil {
			t.Fatal(err)
		}
		if !inspection.Valid || inspection.BaseSeries != profile.series {
			t.Fatalf("%s: %+v", profile.name, inspection)
		}
		for _, metric := range inspection.Metrics {
			original, err := os.ReadFile(filepath.Join(source, metric.File))
			if err != nil {
				t.Fatal(err)
			}
			copied, err := os.ReadFile(filepath.Join(copyRoot, metric.File))
			if err != nil {
				t.Fatal(err)
			}
			if string(original) != string(copied) {
				t.Fatalf("profile %s changed source config %s", profile.name, metric.File)
			}
		}
	}
}
