package dataset

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/golang/snappy"
	"github.com/prometheus/prometheus/prompb"
	"golang.org/x/exp/rand"
	"metrics-bench-suite/pkg/samples"
)

const Format = "prometheus-remote-write-v1-snappy"
const SchemaVersion = 1

// Options fully specifies the sample sequence; milliseconds match the wire format.
type Options struct {
	Profile             string    `json:"profile"`
	Start               time.Time `json:"start"`
	End                 time.Time `json:"end"`
	IntervalMillis      int64     `json:"interval_ms"`
	Seed                uint64    `json:"seed"`
	Replica             int       `json:"replica"`
	ChurnRate           float64   `json:"churn_rate"`
	ChurnIntervalMillis int64     `json:"churn_interval_ms"`
	MaxSamples          int       `json:"max_samples_per_request"`
}

func (o Options) Validate() error {
	if o.Start.IsZero() || o.End.IsZero() || !o.End.After(o.Start) || o.IntervalMillis <= 0 || o.MaxSamples <= 0 || o.Replica < 0 {
		return fmt.Errorf("require start < end, positive interval and batch size, and nonnegative replica")
	}
	if o.Start.Nanosecond()%1e6 != 0 || o.End.Nanosecond()%1e6 != 0 {
		return fmt.Errorf("timestamps must have millisecond precision")
	}
	if o.Start.Year() < 1970 || o.End.Year() > 2261 {
		return fmt.Errorf("supported timestamp years are 1970 through 2261")
	}
	if math.IsNaN(o.ChurnRate) || o.ChurnRate < 0 || o.ChurnRate > 1 || o.ChurnIntervalMillis < 0 || (o.ChurnRate > 0 && o.ChurnIntervalMillis == 0) {
		return fmt.Errorf("churn rate must be in [0,1], with a positive interval when enabled")
	}
	return nil
}

func (o Options) SamplesPerSeries() int64 {
	return 1 + (o.End.UnixMilli()-o.Start.UnixMilli()-1)/o.IntervalMillis
}

// Generator identifies the exact executable as well as its source build information.
type Generator struct {
	ContractVersion int    `json:"contract_version"`
	Revision        string `json:"revision"`
	Modified        bool   `json:"modified"`
	BinarySHA256    string `json:"binary_sha256"`
}

func Identity() (Generator, error) {
	executable, err := os.Executable()
	if err != nil {
		return Generator{}, err
	}
	data, err := os.ReadFile(executable)
	if err != nil {
		return Generator{}, err
	}
	identity := Generator{ContractVersion: SchemaVersion, Revision: "unknown", BinarySHA256: digest(data)}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			switch setting.Key {
			case "vcs.revision":
				identity.Revision = setting.Value
			case "vcs.modified":
				identity.Modified = setting.Value == "true"
			}
		}
	}
	return identity, nil
}

type File struct {
	Name    string `json:"name"`
	Bytes   int64  `json:"bytes"`
	SHA256  string `json:"sha256"`
	Samples int64  `json:"samples"`
}

type Summary struct {
	SchemaVersion             int       `json:"schema_version"`
	Format                    string    `json:"format"`
	DatasetID                 string    `json:"dataset_id"`
	Generator                 Generator `json:"generator"`
	Options                   Options   `json:"options"`
	ConfigSHA256              string    `json:"config_sha256"`
	Metrics                   []Metric  `json:"metrics"`
	BaseSeries                int64     `json:"base_series"`
	ActualSeries              int64     `json:"actual_series"`
	ActualSamples             int64     `json:"actual_samples"`
	RemoteWriteFiles          int       `json:"remote_write_files"`
	RemoteWriteTotalBytes     int64     `json:"remote_write_total_bytes"`
	Files                     []File    `json:"files"`
	GenerationDurationSeconds float64   `json:"generation_duration_seconds"`
}

func (s Summary) identity() string {
	s.DatasetID = ""
	s.GenerationDurationSeconds = 0
	return jsonDigest(s)
}

// Generate streams one base series at a time. It never retains the Cartesian
// product, the whole time range, or a random generator per base series.
func Generate(ctx context.Context, configPath, output string, options Options) (*Summary, error) {
	if err := options.Validate(); err != nil {
		return nil, err
	}
	inspection, err := Inspect(configPath)
	if err != nil {
		return nil, err
	}
	if !inspection.Valid {
		return nil, fmt.Errorf("invalid config: %s", strings.Join(inspection.Errors, "; "))
	}
	if inspection.BaseSeries > math.MaxInt64/options.SamplesPerSeries() {
		return nil, fmt.Errorf("sample count overflow")
	}
	identity, err := Identity()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(output, 0755); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(output)
	if err != nil {
		return nil, err
	}
	if len(entries) != 0 {
		return nil, fmt.Errorf("output directory must be empty: %s", output)
	}
	started := time.Now()
	summary := &Summary{SchemaVersion: SchemaVersion, Format: Format, Generator: identity, Options: options, ConfigSHA256: inspection.ConfigSHA256, Metrics: inspection.Metrics, BaseSeries: inspection.BaseSeries, Files: []File{}}
	batch := prompb.WriteRequest{}
	batchSamples := 0
	flush := func() error {
		if batchSamples == 0 {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		raw, err := batch.Marshal()
		if err != nil {
			return err
		}
		data := snappy.Encode(nil, raw)
		name := fmt.Sprintf("prw-%012d.bin", len(summary.Files))
		if err := os.WriteFile(filepath.Join(output, name), data, 0644); err != nil {
			return err
		}
		summary.Files = append(summary.Files, File{Name: name, Bytes: int64(len(data)), SHA256: digest(data), Samples: int64(batchSamples)})
		summary.RemoteWriteTotalBytes += int64(len(data))
		batch = prompb.WriteRequest{}
		batchSamples = 0
		return nil
	}
	churnCounts := samples.ChurnCounts(inspection.configs, options.ChurnRate)
	for i := range inspection.configs {
		config := &inspection.configs[i]
		labels := make([]samples.LabelCandidates, 0, len(config.Config.Tags))
		for _, tag := range config.Config.Tags {
			labels = append(labels, samples.LabelCandidates{Name: tag.Name, Values: tag.Dist.LabelGenerator().All()})
		}
		seriesIndex := 0
		churnCount := churnCounts[i]
		samples.VisitTagSets(labels, func(series samples.SeriesWithIndex) bool {
			if err = ctx.Err(); err != nil {
				return false
			}
			baseLabels := samples.BuildSeriesLabels(config.Name, series.Series, config.ReplicaInsertIndex, false, 0, options.Replica)
			seedData, _ := json.Marshal(struct {
				Seed   uint64
				Labels []prompb.Label
			}{options.Seed, baseLabels})
			seedHash := sha256.Sum256(seedData)
			generator := config.Config.Fields[0].Dist.FieldGeneratorWithRandom(rand.New(rand.NewSource(binary.LittleEndian.Uint64(seedHash[:8]))))
			previousEpoch := int64(-1)
			var currentLabels []prompb.Label
			for sampleIndex := int64(0); sampleIndex < options.SamplesPerSeries(); sampleIndex++ {
				if err = ctx.Err(); err != nil {
					return false
				}
				timestamp := options.Start.UnixMilli() + sampleIndex*options.IntervalMillis
				epoch := int64(0)
				churn := seriesIndex < churnCount
				if churn {
					epoch = (timestamp - options.Start.UnixMilli()) / options.ChurnIntervalMillis
				}
				if epoch != previousEpoch {
					currentLabels = samples.BuildSeriesLabels(config.Name, series.Series, config.ReplicaInsertIndex, churn, epoch, options.Replica)
					summary.ActualSeries++
				}
				if epoch != previousEpoch || batchSamples == 0 {
					batch.Timeseries = append(batch.Timeseries, prompb.TimeSeries{Labels: currentLabels})
				}
				previousEpoch = epoch
				value := generator.Next()
				if math.IsNaN(value) || math.IsInf(value, 0) {
					err = fmt.Errorf("metric %s produced nonfinite value", config.Name)
					return false
				}
				last := &batch.Timeseries[len(batch.Timeseries)-1]
				last.Samples = append(last.Samples, prompb.Sample{Timestamp: timestamp, Value: value})
				summary.ActualSamples++
				batchSamples++
				if batchSamples == options.MaxSamples {
					if err = flush(); err != nil {
						return false
					}
				}
			}
			seriesIndex++
			return true
		})
		if err != nil {
			return nil, err
		}
	}
	if err := flush(); err != nil {
		return nil, err
	}
	summary.RemoteWriteFiles = len(summary.Files)
	summary.GenerationDurationSeconds = time.Since(started).Seconds()
	summary.DatasetID = summary.identity()
	data, err := json.MarshalIndent(summary, "", "  ")
	if err != nil {
		return nil, err
	}
	// A missing summary marks interrupted generation as incomplete.
	temp := filepath.Join(output, "summary.json.partial")
	if err := os.WriteFile(temp, append(data, '\n'), 0644); err != nil {
		return nil, err
	}
	if err := os.Rename(temp, filepath.Join(output, "summary.json")); err != nil {
		return nil, err
	}
	return summary, nil
}

// Verify decodes each request and checks counts and per-series timestamps using
// bounded memory. Series-major files allow continuing a series across batches.
func Verify(ctx context.Context, root string) (*Summary, error) {
	data, err := os.ReadFile(filepath.Join(root, "summary.json"))
	if err != nil {
		return nil, err
	}
	var summary Summary
	if err := json.Unmarshal(data, &summary); err != nil {
		return nil, err
	}
	if summary.SchemaVersion != SchemaVersion || summary.Format != Format || summary.DatasetID != summary.identity() {
		return nil, fmt.Errorf("unsupported or inconsistent dataset manifest")
	}
	if err := summary.Options.Validate(); err != nil {
		return nil, err
	}
	if summary.ConfigSHA256 != jsonDigest(summary.Metrics) {
		return nil, fmt.Errorf("config inventory digest mismatch")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	if len(entries) != len(summary.Files)+1 || len(summary.Files) == 0 {
		return nil, fmt.Errorf("missing or unexpected dataset files")
	}
	actualSamples, actualSeries, baseSeries, totalBytes := int64(0), int64(0), int64(0), int64(0)
	baseKey, lastKey := "", ""
	seriesSamples := int64(0)
	metricCounts := map[string]int64{}
	finishSeries := func() error {
		if baseKey != "" && seriesSamples != summary.Options.SamplesPerSeries() {
			return fmt.Errorf("incomplete base series: got %d samples", seriesSamples)
		}
		return nil
	}
	for i, file := range summary.Files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if file.Name != fmt.Sprintf("prw-%012d.bin", i) {
			return nil, fmt.Errorf("invalid file order/name: %s", file.Name)
		}
		path := filepath.Join(root, file.Name)
		stat, err := os.Lstat(path)
		if err != nil {
			return nil, err
		}
		if !stat.Mode().IsRegular() {
			return nil, fmt.Errorf("expected regular file: %s", file.Name)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		if int64(len(data)) != file.Bytes || digest(data) != file.SHA256 {
			return nil, fmt.Errorf("checksum or size mismatch: %s", file.Name)
		}
		raw, err := snappy.Decode(nil, data)
		if err != nil {
			return nil, err
		}
		var request prompb.WriteRequest
		if err := request.Unmarshal(raw); err != nil {
			return nil, err
		}
		fileSamples := int64(0)
		for _, series := range request.Timeseries {
			if len(series.Samples) == 0 || len(series.Labels) < 2 {
				return nil, fmt.Errorf("invalid series in %s", file.Name)
			}
			base := make([]prompb.Label, 0, len(series.Labels))
			replicaFound := false
			name := ""
			for j, label := range series.Labels {
				if label.Name == "__name__" {
					name = label.Value
				}
				if j > 0 && label.Name <= series.Labels[j-1].Name {
					return nil, fmt.Errorf("unordered or duplicate labels in %s", file.Name)
				}
				if label.Name == "replica" {
					replicaFound = label.Value == strconv.Itoa(summary.Options.Replica)
				}
				if label.Name != "churn_id" {
					base = append(base, label)
				}
			}
			if !metricName.MatchString(name) {
				return nil, fmt.Errorf("invalid metric name")
			}
			if !replicaFound {
				return nil, fmt.Errorf("missing or incorrect replica label")
			}
			nextBase := jsonDigest(base)
			key := jsonDigest(series.Labels)
			if nextBase != baseKey {
				if err := finishSeries(); err != nil {
					return nil, err
				}
				baseKey = nextBase
				seriesSamples = 0
				baseSeries++
				metricCounts[name]++
			}
			if key != lastKey {
				actualSeries++
				lastKey = key
			}
			for _, sample := range series.Samples {
				expected := summary.Options.Start.UnixMilli() + seriesSamples*summary.Options.IntervalMillis
				if sample.Timestamp != expected || sample.Timestamp >= summary.Options.End.UnixMilli() || math.IsNaN(sample.Value) || math.IsInf(sample.Value, 0) {
					return nil, fmt.Errorf("invalid value or timestamp in %s", file.Name)
				}
				seriesSamples++
				actualSamples++
				fileSamples++
			}
		}
		if fileSamples != file.Samples || fileSamples == 0 || fileSamples > int64(summary.Options.MaxSamples) {
			return nil, fmt.Errorf("invalid sample count in %s", file.Name)
		}
		totalBytes += file.Bytes
	}
	if err := finishSeries(); err != nil {
		return nil, err
	}
	for _, metric := range summary.Metrics {
		if metricCounts[metric.Name] != metric.Series {
			return nil, fmt.Errorf("metric %s series count mismatch", metric.Name)
		}
		delete(metricCounts, metric.Name)
	}
	if len(metricCounts) != 0 || actualSamples != summary.ActualSamples || actualSeries != summary.ActualSeries || baseSeries != summary.BaseSeries || totalBytes != summary.RemoteWriteTotalBytes || len(summary.Files) != summary.RemoteWriteFiles {
		return nil, fmt.Errorf("dataset totals disagree with manifest")
	}
	return &summary, nil
}
