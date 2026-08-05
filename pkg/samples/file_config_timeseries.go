package samples

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/prometheus/prometheus/prompb"
)

// GeneratePermutedTimeSeries generates time series for every label permutation in the file config.
func (f *FileConfig) GeneratePermutedTimeSeries(current time.Time, churnEpoch int64, replica int, out chan<- prompb.TimeSeries) {
	f.GeneratePermutedTimeSeriesContext(context.Background(), current, churnEpoch, replica, out)
}

// GeneratePermutedTimeSeriesContext generates time series until complete or canceled.
func (f *FileConfig) GeneratePermutedTimeSeriesContext(ctx context.Context, current time.Time, churnEpoch int64, replica int, out chan<- prompb.TimeSeries) {
	f.GeneratePermutedTimeSeriesWithLabelValuesContext(ctx, current, churnEpoch, replica, nil, out)
}

// GeneratePermutedTimeSeriesWithLabelValuesContext generates only permutations
// matching the supplied label values. A constrained label must exist in the
// file config and contain the requested value; otherwise it emits no series.
func (f *FileConfig) GeneratePermutedTimeSeriesWithLabelValuesContext(ctx context.Context, current time.Time, churnEpoch int64, replica int, labelValues map[string]string, out chan<- prompb.TimeSeries) {
	labels := make([]LabelCandidates, 0, len(f.Config.Tags))
	for _, tag := range f.Config.Tags {
		values := tag.Dist.LabelGenerator().All()
		if value, ok := labelValues[tag.Name]; ok {
			if !containsLabelValue(values, value) {
				return
			}
			values = []string{value}
		}
		labels = append(labels, LabelCandidates{
			Name:   tag.Name,
			Values: values,
		})
	}
	for labelName := range labelValues {
		if !hasConfigLabel(f.Config.Tags, labelName) {
			return
		}
	}

	seriesIdx := 0
	replicaInsertIndex := f.ReplicaInsertIndex
	if replicaInsertIndex < 0 || replicaInsertIndex > len(f.Config.Tags) {
		replicaInsertIndex = 0
		for _, tag := range f.Config.Tags {
			if tag.Name < "replica" {
				replicaInsertIndex++
			} else {
				break
			}
		}
	}

	tagSetPermutationStream(labels, func(seriesWithIndex SeriesWithIndex) bool {
		if ctx.Err() != nil {
			return false
		}
		ts := f.processSingleFileConfigSeries(seriesWithIndex, seriesIdx, replicaInsertIndex, current, churnEpoch, replica)
		select {
		case out <- ts:
			seriesIdx++
			return true
		case <-ctx.Done():
			return false
		}
	})
}

func containsLabelValue(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func hasConfigLabel(tags []Tag, wanted string) bool {
	for _, tag := range tags {
		if tag.Name == wanted {
			return true
		}
	}
	return false
}

// processSingleFileConfigSeries builds one TimeSeries for a single label-series from a file config.
// seriesIdx is the 0-based index of this series in the permutation stream.
func (f *FileConfig) processSingleFileConfigSeries(
	seriesWithIndex SeriesWithIndex,
	seriesIdx int,
	replicaInsertIndex int,
	current time.Time,
	churnEpoch int64,
	replica int,
) prompb.TimeSeries {
	series := seriesWithIndex.Series
	index := seriesWithIndex.Index

	ts := prompb.TimeSeries{
		Labels: []prompb.Label{
			{
				Name:  "__name__",
				Value: f.Name,
			},
		},
		Samples: make([]prompb.Sample, 0),
	}
	replicaValue := strconv.Itoa(replica)
	replicaInserted := false
	for labelIndex, labelPair := range series {
		if labelIndex == replicaInsertIndex {
			ts.Labels = append(ts.Labels, prompb.Label{
				Name:  "replica",
				Value: replicaValue,
			})
			replicaInserted = true
		}
		if labelPair.Value == "" {
			continue
		}
		ts.Labels = append(ts.Labels, prompb.Label{
			Name:  labelPair.Name,
			Value: labelPair.Value,
		})
	}
	if !replicaInserted && replicaInsertIndex == len(series) {
		ts.Labels = append(ts.Labels, prompb.Label{
			Name:  "replica",
			Value: replicaValue,
		})
	}

	if f.shouldChurn(seriesIdx) {
		churnLabel := prompb.Label{
			Name:  "churn_id",
			Value: fmt.Sprintf("epoch_%d", churnEpoch),
		}
		insertIdx := 1
		for insertIdx < len(ts.Labels) && ts.Labels[insertIdx].Name < churnLabel.Name {
			insertIdx++
		}
		ts.Labels = append(ts.Labels[:insertIdx], append([]prompb.Label{churnLabel}, ts.Labels[insertIdx:]...)...)
	}

	generator := f.GetOrCreateFieldGenerator(index)
	value := generator.Next()

	ts.Samples = append(ts.Samples, prompb.Sample{
		Value:     value,
		Timestamp: current.UnixMilli(),
	})
	return ts
}

func (f *FileConfig) shouldChurn(seriesIdx int) bool {
	if len(f.ChurnIndices) == 0 {
		return false
	}
	i := sort.SearchInts(f.ChurnIndices, seriesIdx)
	return i < len(f.ChurnIndices) && f.ChurnIndices[i] == seriesIdx
}
