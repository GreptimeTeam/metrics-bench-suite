package samples

import (
	"reflect"
	"testing"
	"time"

	"github.com/prometheus/prometheus/prompb"
)

func TestProcessPermutedTimeSeriesChurnRateZero(t *testing.T) {
	fileConfig := buildChurnTestFileConfig()
	timeSeries := collectPermutedTimeSeries(&fileConfig, 5, 0, 0.0)

	if len(timeSeries) != 4 {
		t.Fatalf("expected 4 time series, got %d", len(timeSeries))
	}

	for _, ts := range timeSeries {
		if hasLabel(ts.Labels, "churn_id") {
			t.Fatalf("expected no churn_id label when churn rate is zero")
		}
	}
}

func TestProcessPermutedTimeSeriesChurnDeterministic(t *testing.T) {
	fileConfig := buildChurnTestFileConfig()
	churnEpoch := int64(7)
	timeSeries := collectPermutedTimeSeries(&fileConfig, churnEpoch, 0, 0.0001)

	if len(timeSeries) != 4 {
		t.Fatalf("expected 4 time series, got %d", len(timeSeries))
	}

	churned := 0
	for _, ts := range timeSeries {
		if value, ok := getLabelValue(ts.Labels, "churn_id"); ok {
			churned++
			if value != "epoch_7" {
				t.Fatalf("expected churn_id value %q, got %q", "epoch_7", value)
			}
		}
	}

	if churned != 1 {
		t.Fatalf("expected 1 churned series, got %d", churned)
	}
}

func TestGeneratePermutedTimeSeries_ChurnsDeterministicSubset(t *testing.T) {
	fileConfig := buildChurnTestFileConfig()
	churnEpoch := int64(9)
	// Permuted time series has 4 series; with this churn rate, 2 should churn.
	timeSeries := collectPermutedTimeSeries(&fileConfig, churnEpoch, 0, 0.0002)

	if len(timeSeries) != 4 {
		t.Fatalf("expected 4 time series, got %d", len(timeSeries))
	}

	churned := 0
	for i, ts := range timeSeries {
		_, hasChurn := getLabelValue(ts.Labels, "churn_id")
		if i < 2 {
			if !hasChurn {
				t.Fatalf("expected churn_id for series index %d", i)
			}
			churned++
			continue
		}
		if hasChurn {
			t.Fatalf("expected no churn_id for series index %d", i)
		}
	}

	if churned != 2 {
		t.Fatalf("expected 2 churned series, got %d", churned)
	}

	firstAlpha, _ := getLabelValue(timeSeries[0].Labels, "alpha")
	firstZulu, _ := getLabelValue(timeSeries[0].Labels, "zulu")
	secondAlpha, _ := getLabelValue(timeSeries[1].Labels, "alpha")
	secondZulu, _ := getLabelValue(timeSeries[1].Labels, "zulu")
	if firstAlpha != "a1" || firstZulu != "z1" || secondAlpha != "a2" || secondZulu != "z1" {
		t.Fatalf("unexpected permutation order for churned series")
	}
}

func TestProcessSingleFileConfigSeries_ChurnInsertsSortedLabel(t *testing.T) {
	fileConfig := buildProcessSingleFileConfig()
	series := SeriesWithIndex{
		Series: []LabelPair{
			{Name: "alpha", Value: "a"},
			{Name: "zulu", Value: "z"},
		},
		Index: []int{0, 0},
	}

	ts := fileConfig.processSingleFileConfigSeries(
		series,
		0,
		1,
		time.Unix(0, 0),
		42,
		3,
		0.0001,
	)

	value, ok := getLabelValue(ts.Labels, "churn_id")
	if !ok {
		t.Fatalf("expected churn_id label to be present")
	}
	if value != "epoch_42" {
		t.Fatalf("expected churn_id value %q, got %q", "epoch_42", value)
	}

	expectedOrder := []string{"__name__", "alpha", "churn_id", "replica", "zulu"}
	if !reflect.DeepEqual(labelNames(ts.Labels), expectedOrder) {
		t.Fatalf("expected label order %v, got %v", expectedOrder, labelNames(ts.Labels))
	}
}

func TestProcessSingleFileConfigSeries_NoChurnWhenNotSelected(t *testing.T) {
	fileConfig := buildProcessSingleFileConfig()
	series := SeriesWithIndex{
		Series: []LabelPair{
			{Name: "alpha", Value: "a"},
			{Name: "zulu", Value: "z"},
		},
		Index: []int{0, 0},
	}

	ts := fileConfig.processSingleFileConfigSeries(
		series,
		1,
		1,
		time.Unix(0, 0),
		42,
		3,
		0.0001,
	)

	if hasLabel(ts.Labels, "churn_id") {
		t.Fatalf("expected no churn_id label when series is not selected to churn")
	}
}

func TestProcessSingleFileConfigSeries_NoChurnWhenRateZero(t *testing.T) {
	fileConfig := buildProcessSingleFileConfig()
	series := SeriesWithIndex{
		Series: []LabelPair{
			{Name: "alpha", Value: "a"},
			{Name: "zulu", Value: "z"},
		},
		Index: []int{0, 0},
	}

	ts := fileConfig.processSingleFileConfigSeries(
		series,
		0,
		1,
		time.Unix(0, 0),
		42,
		3,
		0.0,
	)

	if hasLabel(ts.Labels, "churn_id") {
		t.Fatalf("expected no churn_id label when churn rate is zero")
	}
}

func buildChurnTestFileConfig() FileConfig {
	return FileConfig{
		Name: "test_metric",
		Config: Config{
			Tags: []Tag{
				{
					Name: "alpha",
					Dist: Distribution{
						Type: "weighted_preset",
						Preset: []PresetItem{
							{Value: "a1", Weight: 1},
							{Value: "a2", Weight: 1},
						},
					},
				},
				{
					Name: "zulu",
					Dist: Distribution{
						Type: "weighted_preset",
						Preset: []PresetItem{
							{Value: "z1", Weight: 1},
							{Value: "z2", Weight: 1},
						},
					},
				},
			},
			Fields: []Field{
				{
					Name: "value",
					Dist: Distribution{
						Type:       "uniform",
						LowerBound: &[]float64{0.0}[0],
						UpperBound: &[]float64{1.0}[0],
					},
				},
			},
		},
	}
}

func buildProcessSingleFileConfig() FileConfig {
	return FileConfig{
		Name: "test_metric",
		Config: Config{
			Fields: []Field{
				{
					Name: "value",
					Dist: Distribution{
						Type:  "constant_float",
						Value: 1.0,
					},
				},
			},
		},
	}
}

func collectPermutedTimeSeries(fileConfig *FileConfig, churnEpoch int64, replica int, churnRate float64) []prompb.TimeSeries {
	out := make(chan prompb.TimeSeries, 4)
	fileConfig.GeneratePermutedTimeSeries(time.Now(), churnEpoch, replica, churnRate, out)
	close(out)

	var results []prompb.TimeSeries
	for ts := range out {
		results = append(results, ts)
	}
	return results
}

func hasLabel(labels []prompb.Label, name string) bool {
	_, ok := getLabelValue(labels, name)
	return ok
}

func getLabelValue(labels []prompb.Label, name string) (string, bool) {
	for _, label := range labels {
		if label.Name == name {
			return label.Value, true
		}
	}
	return "", false
}

func labelNames(labels []prompb.Label) []string {
	names := make([]string, len(labels))
	for i, label := range labels {
		names[i] = label.Name
	}
	return names
}
