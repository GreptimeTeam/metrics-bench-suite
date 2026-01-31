package samples

import (
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/prometheus/prometheus/prompb"
)

// GeneratePermutedTimeSeries generates time series for every label permutation in the file config.
func (f *FileConfig) GeneratePermutedTimeSeries(current time.Time, churnEpoch int64, replica int, churnRate float64, out chan<- prompb.TimeSeries) {
	tagOrder := f.TagOrder
	if len(tagOrder) != len(f.Config.Tags) {
		tagOrder = make([]int, len(f.Config.Tags))
		for i := range tagOrder {
			tagOrder[i] = i
		}
		sort.Slice(tagOrder, func(i, j int) bool {
			return f.Config.Tags[tagOrder[i]].Name < f.Config.Tags[tagOrder[j]].Name
		})
	}

	labels := make([]LabelCandidates, 0, len(f.Config.Tags))
	for _, tagIndex := range tagOrder {
		tag := f.Config.Tags[tagIndex]
		values := tag.Dist.LabelGenerator().All()
		labels = append(labels, LabelCandidates{
			Name:   tag.Name,
			Values: values,
		})
	}

	seriesIdx := 0
	replicaInsertIndex := f.ReplicaInsertIndex
	if len(f.TagOrder) != len(f.Config.Tags) || replicaInsertIndex < 0 || replicaInsertIndex > len(tagOrder) {
		replicaInsertIndex = 0
		for _, idx := range tagOrder {
			if f.Config.Tags[idx].Name < "replica" {
				replicaInsertIndex++
			} else {
				break
			}
		}
	}

	TagSetPermutationStream(labels, func(seriesWithIndex SeriesWithIndex) {
		ts := f.processSingleFileConfigSeries(seriesWithIndex, seriesIdx, replicaInsertIndex, current, churnEpoch, replica, churnRate)
		out <- ts
		seriesIdx++
	})
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
	churnRate float64,
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

	if churnRate > 0 && shouldChurn(seriesIdx, churnRate) {
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

// shouldChurn determines if a series at the given index should be churned based on the churn rate.
func shouldChurn(seriesIdx int, churnRate float64) bool {
	// Use deterministic selection: series indices that fall within the churn percentage are churned.
	// This ensures consistent selection across generations.
	// We use modulo 10000 for finer granularity (supports churn rates down to 0.01%).
	threshold := int(churnRate * 10000)
	return (seriesIdx % 10000) < threshold
}
