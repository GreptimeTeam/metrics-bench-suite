package sample_loader

import (
	"fmt"
	"log"
	"math"
	"math/rand/v2"
	"metrics-bench-suite/pkg/http"
	"metrics-bench-suite/pkg/samples"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/prometheus/prompb"
	"github.com/spf13/cobra"
)

// LabelPair represents an ordered label name and value pair
type LabelPair struct {
	Name  string
	Value string
}

// SampleLoader is a tool that generate samples from config files and send them to the remote write endpoint.
type SampleLoader struct {
	ConfigPath     string
	RemoteWriteURL string
	StartDate      time.Time
	EndDate        time.Time
	Interval       time.Duration
	Seed           int
	MaxSamples     int
	TickInterval   time.Duration
	Workers        int
	Infinite       bool
	TablePickCount uint64
	DryRun         bool
	Replica        int
	// ChurnRate is the fraction (0.0–1.0) of time series that will be churned at each churn event.
	ChurnRate float64
	// ChurnInterval is the duration between churn events.
	ChurnInterval time.Duration
	// churnEpoch tracks the current churn generation, incremented each ChurnInterval
	churnEpoch int64
	// churnMutex protects access to churnEpoch
	churnMutex sync.RWMutex
	// fieldGeneratorsPerFile stores field generators for each series identified by file name and index combination
	// Keyed by a composite key of file name and index pattern
	fieldGeneratorsPerFile map[string]samples.FloatGenerator
	// mutex to protect access to fieldGeneratorsPerFile map
	fieldGeneratorsMutex sync.RWMutex
}

func (s *SampleLoader) run(cmd *cobra.Command, _ []string) error {
	var err error
	intervalStr, _ := cmd.Flags().GetString("interval")
	initialDateStr, _ := cmd.Flags().GetString("start-date")
	endDateStr, _ := cmd.Flags().GetString("end-date")
	tickIntervalStr, _ := cmd.Flags().GetString("tick-interval")
	s.Interval, err = time.ParseDuration(intervalStr)
	if err != nil {
		return err
	}
	s.ConfigPath, err = cmd.Flags().GetString("config")
	if err != nil {
		return err
	}
	s.StartDate, err = time.Parse(time.RFC3339, initialDateStr)
	if err != nil {
		return err
	}
	s.EndDate, err = time.Parse(time.RFC3339, endDateStr)
	if err != nil {
		return err
	}
	s.DryRun, err = cmd.Flags().GetBool("dry-run")
	if err != nil {
		return err
	}

	// Check for remote-write-url early, only required when not in dry-run mode
	s.RemoteWriteURL, err = cmd.Flags().GetString("remote-write-url")
	if err != nil {
		return err
	}

	// Check if remote-write-url is required (not in dry-run mode)
	if !s.DryRun && s.RemoteWriteURL == "" {
		return fmt.Errorf("remote-write-url is required when not in dry-run mode")
	}
	s.MaxSamples, err = cmd.Flags().GetInt("max-samples")
	if err != nil {
		return err
	}
	s.TickInterval, err = time.ParseDuration(tickIntervalStr)
	if err != nil {
		return err
	}
	s.Workers, err = cmd.Flags().GetInt("workers")
	if err != nil {
		return err
	}
	s.Infinite, err = cmd.Flags().GetBool("infinite")
	if err != nil {
		return err
	}
	s.TablePickCount, err = cmd.Flags().GetUint64("table-pick-count")
	if err != nil {
		return err
	}
	s.Replica, err = cmd.Flags().GetInt("replica")
	if err != nil {
		return err
	}
	s.DryRun, err = cmd.Flags().GetBool("dry-run")
	if err != nil {
		return err
	}

	// Check for remote-write-url early, only required when not in dry-run mode
	s.RemoteWriteURL, err = cmd.Flags().GetString("remote-write-url")
	if err != nil {
		return err
	}

	// Check if remote-write-url is required (not in dry-run mode)
	if !s.DryRun && s.RemoteWriteURL == "" {
		return fmt.Errorf("remote-write-url is required when not in dry-run mode")
	}

	s.ChurnRate, err = cmd.Flags().GetFloat64("churn-rate")
	if err != nil {
		return err
	}
	if s.ChurnRate < 0.0 || s.ChurnRate > 1.0 {
		return fmt.Errorf("churn-rate must be between 0.0 and 1.0")
	}

	churnIntervalStr, _ := cmd.Flags().GetString("churn-interval")
	s.ChurnInterval, err = time.ParseDuration(churnIntervalStr)
	if err != nil {
		return err
	}
	log.Printf("Start date: %s", s.StartDate)
	log.Printf("End date: %s", s.EndDate)
	log.Printf("Interval: %s", s.Interval)
	log.Printf("Tick interval: %s", s.TickInterval)
	log.Printf("Config path: %s", s.ConfigPath)
	log.Printf("Table pick rate: %d", s.TablePickCount)
	log.Printf("Replica label value: %d", s.Replica)
	log.Printf("Dry run: %t", s.DryRun)
	log.Printf("Churn rate: %f", s.ChurnRate)
	log.Printf("Churn interval: %s", s.ChurnInterval)

	fileConfigs, err := samples.WalkAndParseConfigWithMaxFileCount(s.ConfigPath, s.TablePickCount)
	if err != nil {
		return err
	}
	if len(fileConfigs) == 0 {
		return fmt.Errorf("no config files found")
	}

	log.Printf("Generating metrics...")

	// Initialize field generators map
	s.fieldGeneratorsPerFile = make(map[string]samples.FloatGenerator)

	requestChan := make(chan prompb.WriteRequest, s.Workers)

	wg := sync.WaitGroup{}
	for i := 0; i < s.Workers; i++ {
		wg.Add(1)
		go worker(i, s.RemoteWriteURL, requestChan, &wg, s.DryRun)
	}

	current := s.StartDate
	if s.Infinite {
		current = time.Now()
	}

	// Apply a one-time startup jitter in [0, tick_interval)
	var jitter time.Duration
	if s.TickInterval > 0 {
		jitter = time.Duration(rand.Float64() * float64(s.TickInterval))
	}
	log.Printf("Startup jitter: %s", jitter)
	if jitter > 0 {
		time.Sleep(jitter)
	}

	// Track start time for churn epoch calculation
	startTime := time.Now()
	s.churnEpoch = 0

	// First generation immediately after jitter
	s.churnMutex.RLock()
	currentChurnEpoch := s.churnEpoch
	s.churnMutex.RUnlock()
	log.Printf("Generating samples for %s (churn epoch: %d)", current, currentChurnEpoch)
	s.convertToRemoteWriteRequestsStreaming(fileConfigs, current, s.MaxSamples, requestChan, s.ChurnRate, currentChurnEpoch)
	current = current.Add(s.Interval)
	if !s.Infinite {
		if current.After(s.EndDate) {
			log.Printf("End date reached, stopping")
			close(requestChan)
			wg.Wait()
			return nil
		}
	}

	ticker := time.NewTicker(s.TickInterval)
	defer ticker.Stop()

	for range ticker.C {
		// Update churn epoch based on elapsed time
		currentChurnEpoch := s.updateChurnEpoch(startTime)
		log.Printf("Generating samples for %s (churn epoch: %d)", current, currentChurnEpoch)
		s.convertToRemoteWriteRequestsStreaming(fileConfigs, current, s.MaxSamples, requestChan, s.ChurnRate, currentChurnEpoch)
		current = current.Add(s.Interval)
		if !s.Infinite {
			if current.After(s.EndDate) {
				log.Printf("End date reached, stopping")
				break
			}
		}
	}

	close(requestChan)
	wg.Wait()

	return nil
}

func worker(id int, url string, request <-chan prompb.WriteRequest, wg *sync.WaitGroup, dryRun bool) {
	defer wg.Done()
	for request := range request {
		numSeries := len(request.Timeseries)
		if dryRun {
			log.Printf("worker %d (dry-run) would send request with num series: %d", id, numSeries)
		} else {
			now := time.Now()
			r := http.NewRequester(url)
			err := r.Send(request)
			if err != nil {
				log.Printf("worker %d failed to send write request: %v", id, err)
			}
			log.Printf("worker %d sent request in %s, num series: %d", id, time.Since(now), numSeries)
		}
	}
}

// SeriesWithIndex represents a series with its index position
type SeriesWithIndex struct {
	Series []LabelPair
	Index  []int
}

// TagSetPermutationStream calls fn for each label permutation (combination of label values).
func TagSetPermutationStream(labels []samples.LabelCandidates, fn func(SeriesWithIndex)) {
	if len(labels) == 0 {
		fn(SeriesWithIndex{
			Series: make([]LabelPair, 0),
			Index:  make([]int, 0),
		})
		return
	}

	current := make([]int, len(labels))
	end := make([]int, len(labels))
	for i, label := range labels {
		end[i] = len(label.Values)
	}

	for {
		series := make([]LabelPair, 0, len(labels))
		for i, label := range labels {
			series = append(series, LabelPair{
				Name:  label.Name,
				Value: label.Values[current[i]],
			})
		}
		fn(SeriesWithIndex{
			Series: series,
			Index:  append([]int(nil), current...),
		})

		i := 0
		for i < len(current) {
			current[i]++
			if current[i] < end[i] {
				break
			}
			current[i] = 0
			i++
		}
		if i >= len(current) {
			break
		}
	}
}

// generateTimeSeriesForFileConfig generates time series for a single file config using a dedicated goroutine
func (s *SampleLoader) generateTimeSeriesForFileConfig(fileConfig samples.FileConfig, current time.Time, churnRate float64, churnEpoch int64) <-chan prompb.TimeSeries {
	timeSeriesChan := make(chan prompb.TimeSeries, 1) // Buffered to allow the goroutine to start
	go func() {
		defer close(timeSeriesChan)
		s.processFileConfig(fileConfig, current, churnRate, churnEpoch, timeSeriesChan)
	}()
	return timeSeriesChan
}

// processFileConfig processes a single file config and sends generated time series to out.
func (s *SampleLoader) processFileConfig(fileConfig samples.FileConfig, current time.Time, churnRate float64, churnEpoch int64, out chan<- prompb.TimeSeries) {
	tagOrder := fileConfig.TagOrder
	if len(tagOrder) != len(fileConfig.Config.Tags) {
		tagOrder = make([]int, len(fileConfig.Config.Tags))
		for i := range tagOrder {
			tagOrder[i] = i
		}
		sort.Slice(tagOrder, func(i, j int) bool {
			return fileConfig.Config.Tags[tagOrder[i]].Name < fileConfig.Config.Tags[tagOrder[j]].Name
		})
	}

	labels := make([]samples.LabelCandidates, 0, len(fileConfig.Config.Tags))
	for _, tagIndex := range tagOrder {
		tag := fileConfig.Config.Tags[tagIndex]
		values := tag.Dist.LabelGenerator().All()
		labels = append(labels, samples.LabelCandidates{
			Name:   tag.Name,
			Values: values,
		})
	}

	field := fileConfig.Config.Fields[0]

	seriesIdx := 0
	replicaValue := strconv.Itoa(s.Replica)
	replicaInsertIndex := fileConfig.ReplicaInsertIndex
	if len(fileConfig.TagOrder) != len(fileConfig.Config.Tags) || replicaInsertIndex < 0 || replicaInsertIndex > len(tagOrder) {
		replicaInsertIndex = 0
		for _, idx := range tagOrder {
			if fileConfig.Config.Tags[idx].Name < "replica" {
				replicaInsertIndex++
			} else {
				break
			}
		}
	}

	TagSetPermutationStream(labels, func(seriesWithIndex SeriesWithIndex) {
		s.processSingleFileConfigSeries(fileConfig, seriesWithIndex, seriesIdx, replicaValue, replicaInsertIndex, field, current, churnRate, churnEpoch, out)
		seriesIdx++
	})
}

// processSingleFileConfigSeries builds one TimeSeries for a single label-series from a file config
// and sends it on out. seriesIdx is the 0-based index of this series in the permutation stream.
func (s *SampleLoader) processSingleFileConfigSeries(
	fileConfig samples.FileConfig,
	seriesWithIndex SeriesWithIndex,
	seriesIdx int,
	replicaValue string,
	replicaInsertIndex int,
	field samples.Field,
	current time.Time,
	churnRate float64,
	churnEpoch int64,
	out chan<- prompb.TimeSeries,
) {
	series := seriesWithIndex.Series
	index := seriesWithIndex.Index

	ts := prompb.TimeSeries{
		Labels: []prompb.Label{
			{
				Name:  "__name__",
				Value: fileConfig.Name,
			},
		},
		Samples: make([]prompb.Sample, 0),
	}
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

	if churnRate > 0 && s.shouldChurn(seriesIdx, churnRate) {
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

	generator := s.getFieldGeneratorForFileWithChurn(fileConfig.Name, index, field.Dist, churnRate, churnEpoch, seriesIdx)
	value := generator.Next()

	ts.Samples = append(ts.Samples, prompb.Sample{
		Value:     value,
		Timestamp: current.UnixMilli(),
	})

	out <- ts
}

// shouldChurn determines if a series at the given index should be churned based on the churn rate
func (s *SampleLoader) shouldChurn(seriesIdx int, churnRate float64) bool {
	// Use deterministic selection: series indices that fall within the churn percentage are churned
	// This ensures consistent selection across generations
	// We use modulo 10000 for finer granularity (supports churn rates down to 0.01%)
	threshold := int(churnRate * 10000)
	return (seriesIdx % 10000) < threshold
}

// updateChurnEpoch calculates and updates the current churn epoch based on elapsed time since start
// and return the current churn epoch
func (s *SampleLoader) updateChurnEpoch(startTime time.Time) int64 {
	if s.ChurnInterval <= 0 || s.ChurnRate <= 0 {
		return 0
	}
	elapsed := time.Since(startTime)
	newEpoch := int64(elapsed / s.ChurnInterval)
	s.churnMutex.RLock()
	currentEpoch := s.churnEpoch
	s.churnMutex.RUnlock()

	if newEpoch == currentEpoch {
		// No change in churn epoch, return the current epoch
		return currentEpoch
	} else {
		log.Printf("Churn epoch changed from %d to %d", s.churnEpoch, newEpoch)
		s.churnMutex.Lock()
		s.churnEpoch = newEpoch
		s.churnMutex.Unlock()
		return newEpoch
	}
}

func (s *SampleLoader) convertToRemoteWriteRequestsStreaming(fileConfigs []samples.FileConfig, current time.Time, maxSamples int, requestChan chan<- prompb.WriteRequest, churnRate float64, churnEpoch int64) {
	// Create a combined channel that merges all time series from all file configs
	timeSeriesChan := make(chan prompb.TimeSeries, len(fileConfigs))

	var wg sync.WaitGroup
	// Start a goroutine for each file config
	for _, fileConfig := range fileConfigs {
		wg.Add(1)
		go func(fc samples.FileConfig) {
			defer wg.Done()
			// Get the time series channel for this file config
			tsChan := s.generateTimeSeriesForFileConfig(fc, current, churnRate, churnEpoch)
			// Forward all time series to the main channel
			for ts := range tsChan {
				timeSeriesChan <- ts
			}
		}(fileConfig)
	}

	// Close the main channel when all goroutines are done
	go func() {
		wg.Wait()
		close(timeSeriesChan)
	}()

	// Collect time series and send in batches
	tsSet := make([]prompb.TimeSeries, 0, maxSamples)
	for ts := range timeSeriesChan {
		tsSet = append(tsSet, ts)
		if len(tsSet) >= maxSamples {
			// Send a batch when we reach maxSamples
			requestChan <- prompb.WriteRequest{
				Timeseries: tsSet,
			}
			tsSet = make([]prompb.TimeSeries, 0, maxSamples) // Reset the slice
		}
	}

	// Send any remaining time series
	if len(tsSet) > 0 {
		requestChan <- prompb.WriteRequest{
			Timeseries: tsSet,
		}
	}
}

// convertIndexToKey converts a label index array to a string key for map indexing
func convertIndexToKey(indices []int) string {
	key := ""
	for i, idx := range indices {
		if i > 0 {
			key += ","
		}
		key += fmt.Sprintf("%d", idx)
	}
	return key
}

// getFieldGeneratorForFileWithChurn returns a field generator for a specific series, incorporating churn info for churned series
func (s *SampleLoader) getFieldGeneratorForFileWithChurn(fileName string, indices []int, dist samples.Distribution, churnRate float64, churnEpoch int64, seriesIdx int) samples.FloatGenerator {
	s.fieldGeneratorsMutex.Lock()
	defer s.fieldGeneratorsMutex.Unlock()

	// Create a composite key from file name and indices
	// For churned series, include the epoch in the key so they get fresh generators each epoch
	compositeKey := fileName + ":" + convertIndexToKey(indices)
	if churnRate > 0 && s.shouldChurn(seriesIdx, churnRate) {
		compositeKey = fmt.Sprintf("%s:churn:%d", compositeKey, churnEpoch)
	}

	// Check if generator already exists for this combination
	if generator, exists := s.fieldGeneratorsPerFile[compositeKey]; exists {
		return generator
	}

	// Create new generator and store it
	generator := dist.FieldGenerator()
	s.fieldGeneratorsPerFile[compositeKey] = generator
	return generator
}

func NewCommand() *cobra.Command {
	sampleLoader := &SampleLoader{}

	var rootCmd = &cobra.Command{
		Use:   "sample_loader",
		Short: "SampleLoader is a tool to load samples from a file",
		Run: func(cmd *cobra.Command, args []string) {
			if err := sampleLoader.run(cmd, args); err != nil {
				log.Fatalf("Error: %v", err)
			}
		},
	}

	rootCmd.Flags().StringP("config", "c", "", "The path to the config file")
	rootCmd.Flags().StringP("remote-write-url", "u", "", "The remote write url")
	rootCmd.Flags().StringP("start-date", "", "2025-01-01T00:00:00Z", "The start date of the data")
	rootCmd.Flags().StringP("end-date", "", "2025-01-01T00:01:00Z", "The end date of the data")
	rootCmd.Flags().StringP("interval", "", "30s", "The interval of the data")
	rootCmd.Flags().IntP("max-samples", "s", 20000, "The max number of metrics to load")
	rootCmd.Flags().StringP("tick-interval", "t", "30s", "The interval of the requests")
	rootCmd.Flags().IntP("workers", "w", 1, "The number of workers to send requests")
	rootCmd.Flags().IntP("replica", "r", 0, "The replica tab value of current instance")
	rootCmd.Flags().BoolP("infinite", "i", false, "Run indefinitely")
	rootCmd.Flags().Uint64P("table-pick-count", "n", math.MaxUint64, "The number of tables to pick from")
	rootCmd.Flags().Bool("dry-run", false, "Run in dry-run mode without sending requests")
	rootCmd.Flags().Float64("churn-rate", 0.0, "The rate of time series to churn (0.0-1.0, e.g., 0.01 = 1%)")
	rootCmd.Flags().String("churn-interval", "0s", "The interval at which churn occurs (e.g., 10m)")

	return rootCmd
}
