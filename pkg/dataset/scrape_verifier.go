package dataset

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"

	"github.com/prometheus/prometheus/prompb"
	"metrics-bench-suite/pkg/samples"
)

// scrapeVerifier retains the first scrape's base identities, not sample history
// or historical churn identities. Every subsequent scrape must repeat that order.
type scrapeVerifier struct {
	summary      *Summary
	identities   [][sha256.Size]byte
	seen         map[[sha256.Size]byte]struct{}
	churnCounts  []int
	metricCounts map[string]int64
	metricIndex  int
	metricSeries int64
	samples      int64
	actualSeries int64
}

func newScrapeVerifier(summary *Summary) (*scrapeVerifier, error) {
	if summary.BaseSeries > int64(math.MaxInt) {
		return nil, fmt.Errorf("base series count exceeds platform capacity")
	}
	configs := make([]samples.FileConfig, len(summary.Metrics))
	total := int64(0)
	for i, metric := range summary.Metrics {
		if metric.Series <= 0 || metric.Series > summary.BaseSeries-total ||
			(i > 0 && metric.Name <= summary.Metrics[i-1].Name) {
			return nil, fmt.Errorf("invalid metric inventory")
		}
		total += metric.Series
		configs[i] = samples.FileConfig{Name: metric.Name, SeriesCount: int(metric.Series)}
	}
	if total != summary.BaseSeries {
		return nil, fmt.Errorf("metric inventory disagrees with base series count")
	}
	return &scrapeVerifier{
		summary: summary, seen: make(map[[sha256.Size]byte]struct{}),
		churnCounts:  samples.ChurnCounts(configs, summary.Options.ChurnRate),
		metricCounts: make(map[string]int64),
	}, nil
}

func (v *scrapeVerifier) observe(base []prompb.Label, series prompb.TimeSeries, name string) error {
	o := v.summary.Options
	scrape := v.samples / v.summary.BaseSeries
	position := v.samples % v.summary.BaseSeries
	if scrape >= o.SamplesPerSeries() || len(series.Samples) != 1 {
		return fmt.Errorf("expected one sample per series entry within the dataset window")
	}
	sample := series.Samples[0]
	if sample.Timestamp != o.Start.UnixMilli()+scrape*o.IntervalMillis || math.IsNaN(sample.Value) || math.IsInf(sample.Value, 0) {
		return fmt.Errorf("invalid scrape timestamp or value")
	}
	metric := v.summary.Metrics[v.metricIndex]
	if name != metric.Name {
		return fmt.Errorf("expected metric %s at scrape position %d", metric.Name, position)
	}
	encoded, _ := json.Marshal(base)
	identity := sha256.Sum256(encoded)
	if scrape == 0 {
		if _, exists := v.seen[identity]; exists {
			return fmt.Errorf("duplicate base series in scrape")
		}
		v.seen[identity] = struct{}{}
		v.identities = append(v.identities, identity)
		v.metricCounts[name]++
	} else if identity != v.identities[position] {
		return fmt.Errorf("base series changed or reordered at scrape position %d", position)
	}
	churn := v.metricSeries < int64(v.churnCounts[v.metricIndex])
	epoch, previousEpoch := int64(0), int64(0)
	expectedChurn := ""
	if churn {
		epoch = scrape * o.IntervalMillis / o.ChurnIntervalMillis
		if scrape > 0 {
			previousEpoch = (scrape - 1) * o.IntervalMillis / o.ChurnIntervalMillis
		}
		expectedChurn = fmt.Sprintf("epoch_%d", epoch)
	}
	foundChurn := false
	for _, label := range series.Labels {
		if label.Name == "churn_id" {
			foundChurn = true
			if !churn || label.Value != expectedChurn {
				return fmt.Errorf("unexpected churn identity")
			}
		}
	}
	if foundChurn != churn {
		return fmt.Errorf("missing churn identity")
	}
	if scrape == 0 || (churn && epoch != previousEpoch) {
		v.actualSeries++
	}
	v.samples++
	v.metricSeries++
	if v.metricSeries == metric.Series {
		v.metricSeries = 0
		v.metricIndex++
	}
	if position == v.summary.BaseSeries-1 {
		v.metricIndex = 0
		v.seen = nil // Uniqueness is established; subsequent scrapes compare by position.
	}
	return nil
}

func (v *scrapeVerifier) finish() error {
	if v.samples != v.summary.BaseSeries*v.summary.Options.SamplesPerSeries() {
		return fmt.Errorf("incomplete scrape sequence")
	}
	return nil
}
