package samples

import (
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
