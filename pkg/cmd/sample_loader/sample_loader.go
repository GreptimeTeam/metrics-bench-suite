package sample_loader

import (
	"fmt"
	"log"
	"math"
	"math/rand/v2"
	"metrics-bench-suite/pkg/http"
	"metrics-bench-suite/pkg/samples"
	"sync"
	"time"

	"github.com/prometheus/prometheus/prompb"
	"github.com/spf13/cobra"
)

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
	TagsPickRate   float32
	TablePickCount uint64
	Database       string
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
	s.RemoteWriteURL, err = cmd.Flags().GetString("remote-write-url")
	if err != nil {
		return err
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
	s.TagsPickRate, err = cmd.Flags().GetFloat32("tags-pick-rate")
	if err != nil {
		return err
	}
	s.TablePickCount, err = cmd.Flags().GetUint64("table-pick-count")
	if err != nil {
		return err
	}
	s.Database, err = cmd.Flags().GetString("database")
	if err != nil {
		return err
	}
	log.Printf("Start date: %s", s.StartDate)
	log.Printf("End date: %s", s.EndDate)
	log.Printf("Interval: %s", s.Interval)
	log.Printf("Tick interval: %s", s.TickInterval)
	log.Printf("Config path: %s", s.ConfigPath)
	log.Printf("Tags pick rate: %f", s.TagsPickRate)
	log.Printf("Table pick rate: %d", s.TablePickCount)
	log.Printf("Database: %s", s.Database)

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
		go worker(i, s.RemoteWriteURL, requestChan, &wg)
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

	// First generation immediately after jitter
	log.Printf("Generating samples for %s", current)
	s.convertToRemoteWriteRequestsStreaming(fileConfigs, current, s.MaxSamples, requestChan, s.TagsPickRate)
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
		log.Printf("Generating samples for %s", current)
		s.convertToRemoteWriteRequestsStreaming(fileConfigs, current, s.MaxSamples, requestChan, s.TagsPickRate)
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

func worker(id int, url string, request <-chan prompb.WriteRequest, wg *sync.WaitGroup) {
	defer wg.Done()
	for request := range request {
		numSeries := len(request.Timeseries)
		now := time.Now()
		r := http.NewRequester(url)
		err := r.Send(request)
		if err != nil {
			log.Printf("worker %d failed to send write request: %v", id, err)
		}
		log.Printf("worker %d sent request in %s, num series: %d", id, time.Since(now), numSeries)
	}
}

// SeriesWithIndex represents a series with its index position
type SeriesWithIndex struct {
	Series map[string]string
	Index  []int
}

// TagSetPermutationStream generates permutations on-demand using a goroutine
func TagSetPermutationStream(labels []samples.LabelCandidates, permChan chan<- SeriesWithIndex, totalCount *int) {
	defer close(permChan)
	if len(labels) == 0 {
		permChan <- SeriesWithIndex{
			Series: make(map[string]string),
			Index:  make([]int, 0),
		}
		*totalCount++
		return
	}

	current := make([]int, len(labels))
	end := make([]int, len(labels))
	for i, label := range labels {
		end[i] = len(label.Values)
	}

	// Generate all combinations
	for {
		// Create the current combination
		series := make(map[string]string)
		for i, label := range labels {
			series[label.Name] = label.Values[current[i]]
		}
		permChan <- SeriesWithIndex{
			Series: series,
			Index:  append([]int(nil), current...), // copy the current index
		}
		*totalCount++

		// Increment the combination like counting in base-n
		i := 0
		for i < len(current) {
			current[i]++
			if current[i] < end[i] {
				break
			}
			current[i] = 0
			i++
		}

		// Check if we've exhausted all combinations
		if i >= len(current) {
			break
		}
	}
}

// generateTimeSeriesForFileConfig generates time series for a single file config using a dedicated goroutine
func (s *SampleLoader) generateTimeSeriesForFileConfig(fileConfig samples.FileConfig, current time.Time, pickRate float32) <-chan prompb.TimeSeries {
	timeSeriesChan := make(chan prompb.TimeSeries, 1) // Buffered to allow the goroutine to start

	go func() {
		defer close(timeSeriesChan)

		labels := make([]samples.LabelCandidates, 0)
		for _, tag := range fileConfig.Config.Tags {
			values := tag.Dist.LabelGenerator().All()
			labels = append(labels, samples.LabelCandidates{
				Name:   tag.Name,
				Values: values,
			})
		}

		// Create a channel for the permutations
		permChan := make(chan SeriesWithIndex, 1)

		// Start a goroutine to generate permutations
		go func() {
			totalCount := 0
			TagSetPermutationStream(labels, permChan, &totalCount)
		}()

		field := fileConfig.Config.Fields[0]

		// Process each series one by one
		for seriesWithIndex := range permChan {
			series := seriesWithIndex.Series
			index := seriesWithIndex.Index

			// Create a single time series for this specific tag combination
			ts := prompb.TimeSeries{
				Labels:  make([]prompb.Label, 0),
				Samples: make([]prompb.Sample, 0),
			}
			ts.Labels = append(ts.Labels, prompb.Label{
				Name:  "__name__",
				Value: fileConfig.Name,
			})
			for k, v := range series {
				if pickRate < 1.0 {
					if rand.Float32() > pickRate {
						continue
					}
				}
				ts.Labels = append(ts.Labels, prompb.Label{
					Name:  k,
					Value: v,
				})
			}

			// Get or create a field generator for this specific series using the index
			generator := s.getFieldGeneratorForFile(fileConfig.Name, index, field.Dist)
			value := generator.Next()

			ts.Samples = append(ts.Samples, prompb.Sample{
				Value:     value,
				Timestamp: current.UnixMilli(),
			})

			// Add database label if specified
			if s.Database != "" {
				ts.Labels = append(ts.Labels, prompb.Label{
					Name:  "database",
					Value: s.Database,
				})
			}

			// Send this single time series to the channel
			timeSeriesChan <- ts
		}
	}()

	return timeSeriesChan
}

func (s *SampleLoader) convertToRemoteWriteRequestsStreaming(fileConfigs []samples.FileConfig, current time.Time, maxSamples int, requestChan chan<- prompb.WriteRequest, pickRate float32) {
	// Create a combined channel that merges all time series from all file configs
	timeSeriesChan := make(chan prompb.TimeSeries, len(fileConfigs))

	var wg sync.WaitGroup
	// Start a goroutine for each file config
	for _, fileConfig := range fileConfigs {
		wg.Add(1)
		go func(fc samples.FileConfig) {
			defer wg.Done()
			// Get the time series channel for this file config
			tsChan := s.generateTimeSeriesForFileConfig(fc, current, pickRate)
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

// getFieldGeneratorForFile returns a field generator for a specific series in a file config, creating it if it doesn't exist
func (s *SampleLoader) getFieldGeneratorForFile(fileName string, indices []int, dist samples.Distribution) samples.FloatGenerator {
	s.fieldGeneratorsMutex.Lock()
	defer s.fieldGeneratorsMutex.Unlock()

	// Create a composite key from file name and indices
	compositeKey := fileName + ":" + convertIndexToKey(indices)

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
	rootCmd.Flags().BoolP("infinite", "i", false, "Run indefinitely")
	rootCmd.Flags().Float32P("tags-pick-rate", "p", 1.0, "The rate of the pick tags")
	rootCmd.Flags().Uint64P("table-pick-count", "n", math.MaxUint64, "The number of tables to pick from")
	rootCmd.Flags().StringP("database", "d", "", "The database name to add as a label to all metrics")

	return rootCmd
}
