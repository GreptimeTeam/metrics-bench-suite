package dataset_test

import (
	"context"
	"crypto/sha256"
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
	previousTimestamp := int64(-1)
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
			if len(series.Samples) != 1 || series.Samples[0].Timestamp < previousTimestamp {
				t.Fatal("wire samples are not timestamp-major")
			}
			previousTimestamp = series.Samples[0].Timestamp
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
		if count == 0 || count > limit || (path != paths[len(paths)-1] && count != limit) {
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
			if s.SampleOrder != dataset.TimestampMajor {
				t.Fatal("missing timestamp-major contract")
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
			for _, limit := range []int{1, 16, 19, 128} {
				options.MaxSamples = limit
				optionsDir := t.TempDir()
				if _, err := dataset.Generate(context.Background(), config, optionsDir, options); err != nil {
					t.Fatal(err)
				}
				if _, err := dataset.Verify(context.Background(), optionsDir); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(decoded, decode(t, optionsDir, limit)) {
					t.Fatalf("batch size %d changed logical samples", limit)
				}
			}
			options.Seed++
			different := t.TempDir()
			if _, err := dataset.Generate(context.Background(), config, different, options); err != nil {
				t.Fatal(err)
			}
			if reflect.DeepEqual(decoded, decode(t, different, options.MaxSamples)) {
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

func TestVerifyRejectsInvalidSampleOrder(t *testing.T) {
	config := fixture(t)
	start := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, scenario := range []struct {
		name  string
		order string
		omit  bool
	}{
		{name: "missing", omit: true},
		{name: "empty"},
		{name: "unknown", order: "unknown"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			output := t.TempDir()
			summary, err := dataset.Generate(context.Background(), config, output, dataset.Options{
				Start: start, End: start.Add(time.Second), IntervalMillis: 1000, MaxSamples: 10,
			})
			if err != nil {
				t.Fatal(err)
			}
			summary.SampleOrder = scenario.order
			// Recompute the canonical identity to reach ordering validation.
			summary.DatasetID, summary.GenerationDurationSeconds = "", 0
			identity, err := json.Marshal(summary)
			if err != nil {
				t.Fatal(err)
			}
			summary.DatasetID = fmt.Sprintf("%x", sha256.Sum256(identity))
			manifest, err := json.Marshal(summary)
			if err != nil {
				t.Fatal(err)
			}
			if scenario.omit {
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(manifest, &fields); err != nil {
					t.Fatal(err)
				}
				delete(fields, "sample_order")
				manifest, err = json.Marshal(fields)
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(output, "summary.json"), manifest, 0644); err != nil {
				t.Fatal(err)
			}
			if _, err := dataset.Verify(context.Background(), output); err == nil || !strings.Contains(err.Error(), "unsupported sample order") {
				t.Fatalf("expected unsupported sample order, got %v", err)
			}
		})
	}
}

func TestVerifyRejectsInvalidScrapes(t *testing.T) {
	config := fixture(t)
	start := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	options := dataset.Options{Start: start, End: start.Add(11 * time.Second), IntervalMillis: 2000, Seed: 42, Replica: 7, ChurnRate: 0.5, ChurnIntervalMillis: 4000, MaxSamples: 19}
	for _, scenario := range []struct {
		name string
		edit func([]prompb.TimeSeries) []prompb.TimeSeries
	}{
		{"duplicate_first_scrape", func(series []prompb.TimeSeries) []prompb.TimeSeries {
			series[1] = series[0]
			return series
		}},
		{"duplicate_later_scrape", func(series []prompb.TimeSeries) []prompb.TimeSeries {
			series[17] = series[16]
			return series
		}},
		{"reordered_across_files", func(series []prompb.TimeSeries) []prompb.TimeSeries {
			series[18], series[19] = series[19], series[18]
			return series
		}},
		{"missing_sample", func(series []prompb.TimeSeries) []prompb.TimeSeries {
			return append(series[:20], series[21:]...)
		}},
		{"missing_final_scrape", func(series []prompb.TimeSeries) []prompb.TimeSeries {
			return series[:len(series)-16]
		}},
		{"incorrect_timestamp", func(series []prompb.TimeSeries) []prompb.TimeSeries {
			series[20].Samples[0].Timestamp++
			return series
		}},
		{"incorrect_churn_epoch", func(series []prompb.TimeSeries) []prompb.TimeSeries {
			for i := range series[32].Labels {
				if series[32].Labels[i].Name == "churn_id" {
					series[32].Labels[i].Value = "epoch_0"
				}
			}
			return series
		}},
		{"missing_churn_label", func(series []prompb.TimeSeries) []prompb.TimeSeries {
			for i, label := range series[0].Labels {
				if label.Name == "churn_id" {
					series[0].Labels = append(series[0].Labels[:i], series[0].Labels[i+1:]...)
					break
				}
			}
			return series
		}},
		{"multiple_samples_per_entry", func(series []prompb.TimeSeries) []prompb.TimeSeries {
			series[0].Samples = append(series[0].Samples, series[16].Samples...)
			return append(series[:16], series[17:]...)
		}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			original := t.TempDir()
			summary, err := dataset.Generate(context.Background(), config, original, options)
			if err != nil {
				t.Fatal(err)
			}
			var series []prompb.TimeSeries
			for _, file := range summary.Files {
				data, err := os.ReadFile(filepath.Join(original, file.Name))
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
				series = append(series, request.Timeseries...)
			}
			series = scenario.edit(series)
			// Rewrite all integrity metadata so rejection must come from semantic
			// verification, not a stale checksum, file inventory, or sample total.
			output := t.TempDir()
			summary.Files = nil
			summary.ActualSamples, summary.RemoteWriteTotalBytes = 0, 0
			for len(series) > 0 {
				end, count := 0, int64(0)
				for end < len(series) && count+int64(len(series[end].Samples)) <= int64(options.MaxSamples) {
					count += int64(len(series[end].Samples))
					end++
				}
				request := prompb.WriteRequest{Timeseries: series[:end]}
				raw, err := request.Marshal()
				if err != nil {
					t.Fatal(err)
				}
				data := snappy.Encode(nil, raw)
				file := dataset.File{Name: fmt.Sprintf("prw-%012d.bin", len(summary.Files)), Bytes: int64(len(data)), SHA256: fmt.Sprintf("%x", sha256.Sum256(data)), Samples: count}
				if err := os.WriteFile(filepath.Join(output, file.Name), data, 0644); err != nil {
					t.Fatal(err)
				}
				summary.Files = append(summary.Files, file)
				summary.ActualSamples += count
				summary.RemoteWriteTotalBytes += file.Bytes
				series = series[end:]
			}
			summary.RemoteWriteFiles = len(summary.Files)
			summary.DatasetID, summary.GenerationDurationSeconds = "", 0
			identity, err := json.Marshal(summary)
			if err != nil {
				t.Fatal(err)
			}
			summary.DatasetID = fmt.Sprintf("%x", sha256.Sum256(identity))
			manifest, err := json.Marshal(summary)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(output, "summary.json"), manifest, 0644); err != nil {
				t.Fatal(err)
			}
			if _, err := dataset.Verify(context.Background(), output); err == nil {
				t.Fatal("invalid scrape sequence was accepted")
			} else if strings.Contains(err.Error(), "checksum") || strings.Contains(err.Error(), "manifest") {
				t.Fatalf("test did not reach semantic verification: %v", err)
			}
		})
	}
}
