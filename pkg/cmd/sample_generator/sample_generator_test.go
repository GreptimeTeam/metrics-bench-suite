package samplegenerator

import (
	"testing"

	"github.com/prometheus/prometheus/prompb"
)

func TestSplitTimeSeriesBySampleCountLimitsEveryBatch(t *testing.T) {
	timeSeries := []prompb.TimeSeries{
		{
			Labels:  []prompb.Label{{Name: "__name__", Value: "first"}},
			Samples: []prompb.Sample{{Value: 1}, {Value: 2}, {Value: 3}},
		},
		{
			Labels:  []prompb.Label{{Name: "__name__", Value: "second"}},
			Samples: []prompb.Sample{{Value: 4}, {Value: 5}},
		},
	}

	batches := splitTimeSeriesBySampleCount(timeSeries, 2)
	if len(batches) != 3 {
		t.Fatalf("batch count = %d, want 3", len(batches))
	}
	totalSamples := 0
	for i, batch := range batches {
		batchSamples := 0
		for _, series := range batch {
			batchSamples += len(series.Samples)
		}
		if batchSamples > 2 {
			t.Fatalf("batch %d has %d samples, want at most 2", i, batchSamples)
		}
		totalSamples += batchSamples
	}
	if totalSamples != 5 {
		t.Fatalf("total samples = %d, want 5", totalSamples)
	}
}
