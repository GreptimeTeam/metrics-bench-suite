package sampleloader

import (
	"encoding/base64"
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
	TablePickCount uint64
	DryRun         bool
	Replica        int
	Username       string
	Password       string
	// ChurnRate is the fraction (0.0–1.0) of time series that will be churned at each churn event.
	ChurnRate float64
	// ChurnInterval is the duration between churn events.
	ChurnInterval time.Duration
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
	s.Username, err = cmd.Flags().GetString("username")
	if err != nil {
		return err
	}
	s.Password, err = cmd.Flags().GetString("password")
	if err != nil {
		return err
	}
	if (s.Username == "") != (s.Password == "") {
		return fmt.Errorf("username and password must be provided together")
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
	log.Printf("Authorization enabled: %t", s.authorizationHeader() != "")
	log.Printf("Churn rate: %f", s.ChurnRate)
	log.Printf("Churn interval: %s", s.ChurnInterval)

	fileConfigs, err := samples.WalkAndParseConfigWithMaxFileCount(s.ConfigPath, s.TablePickCount)
	if err != nil {
		return err
	}
	if len(fileConfigs) == 0 {
		return fmt.Errorf("no config files found")
	}

	samples.AssignChurnIndices(fileConfigs, s.ChurnRate)

	log.Printf("Generating metrics...")

	requestChan := make(chan prompb.WriteRequest, s.Workers)

	wg := sync.WaitGroup{}
	for i := 0; i < s.Workers; i++ {
		wg.Add(1)
		go worker(i, s.RemoteWriteURL, s.authorizationHeader(), requestChan, &wg, s.DryRun)
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
	churnEpochGenerator := samples.NewChurnEpochGenerator(s.ChurnInterval)
	currentEpoch := churnEpochGenerator.GetChurnEpoch()

	// First generation immediately after jitter
	log.Printf("Generating samples for %s (churn epoch: %d)", current, currentEpoch)
	s.convertToRemoteWriteRequestsStreaming(fileConfigs, current, requestChan, currentEpoch)
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
		newEpoch := churnEpochGenerator.GetChurnEpoch()
		if newEpoch != currentEpoch {
			currentEpoch = newEpoch
		}
		log.Printf("Generating samples for %s (churn epoch: %d)", current, currentEpoch)
		s.convertToRemoteWriteRequestsStreaming(fileConfigs, current, requestChan, currentEpoch)
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

func (s *SampleLoader) authorizationHeader() string {
	if s.Username == "" && s.Password == "" {
		return ""
	}
	authInfo := fmt.Sprintf("%s:%s", s.Username, s.Password)
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(authInfo))
}

func worker(id int, url string, authorizationHeader string, request <-chan prompb.WriteRequest, wg *sync.WaitGroup, dryRun bool) {
	defer wg.Done()
	for request := range request {
		numSeries := len(request.Timeseries)
		if dryRun {
			log.Printf("worker %d (dry-run) would send request with num series: %d", id, numSeries)
		} else {
			now := time.Now()
			r := http.NewRequester(url)
			if authorizationHeader != "" {
				r.SetHeader("Authorization", authorizationHeader)
			}
			err := r.Send(request)
			if err != nil {
				log.Printf("worker %d failed to send write request: %v", id, err)
			}
			log.Printf("worker %d sent request in %s, num series: %d", id, time.Since(now), numSeries)
		}
	}
}

// generateTimeSeriesForFileConfig generates time series for a single file config using a dedicated goroutine
func (s *SampleLoader) generateTimeSeriesForFileConfig(fileConfig *samples.FileConfig, currentTime time.Time, churnEpoch int64) <-chan prompb.TimeSeries {
	timeSeriesChan := make(chan prompb.TimeSeries, 1) // Buffered to allow the goroutine to start
	go func() {
		defer close(timeSeriesChan)
		fileConfig.GeneratePermutedTimeSeries(currentTime, churnEpoch, s.Replica, timeSeriesChan)
	}()
	return timeSeriesChan
}

func (s *SampleLoader) convertToRemoteWriteRequestsStreaming(fileConfigs []samples.FileConfig, currentTime time.Time, requestChan chan<- prompb.WriteRequest, churnEpoch int64) {
	// Create a combined channel that merges all time series from all file configs
	timeSeriesChan := make(chan prompb.TimeSeries, len(fileConfigs))

	var wg sync.WaitGroup
	// Start a goroutine for each file config
	for i := range fileConfigs {
		wg.Add(1)
		go func(fc *samples.FileConfig) {
			defer wg.Done()
			// Get the time series channel for this file config
			tsChan := s.generateTimeSeriesForFileConfig(fc, currentTime, churnEpoch)
			// Forward all time series to the main channel
			for ts := range tsChan {
				timeSeriesChan <- ts
			}
		}(&fileConfigs[i])
	}

	// Close the main channel when all goroutines are done
	go func() {
		wg.Wait()
		close(timeSeriesChan)
	}()

	// Collect time series and send in batches
	tsSet := make([]prompb.TimeSeries, 0, s.MaxSamples)
	for ts := range timeSeriesChan {
		tsSet = append(tsSet, ts)
		if len(tsSet) >= s.MaxSamples {
			// Send a batch when we reach maxSamples
			requestChan <- prompb.WriteRequest{
				Timeseries: tsSet,
			}
			tsSet = make([]prompb.TimeSeries, 0, s.MaxSamples) // Reset the slice
		}
	}

	// Send any remaining time series
	if len(tsSet) > 0 {
		requestChan <- prompb.WriteRequest{
			Timeseries: tsSet,
		}
	}
}

// NewCommand creates the sample loader command.
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
	rootCmd.Flags().String("username", "", "The username for HTTP Basic authorization")
	rootCmd.Flags().String("password", "", "The password for HTTP Basic authorization")
	rootCmd.Flags().BoolP("infinite", "i", false, "Run indefinitely")
	rootCmd.Flags().Uint64P("table-pick-count", "n", math.MaxUint64, "The number of tables to pick from")
	rootCmd.Flags().Bool("dry-run", false, "Run in dry-run mode without sending requests")
	rootCmd.Flags().Float64("churn-rate", 0.0, "The rate of time series to churn (0.0-1.0, e.g., 0.01 = 1%)")
	rootCmd.Flags().String("churn-interval", "0s", "The interval at which churn occurs (e.g., 10m)")

	return rootCmd
}
