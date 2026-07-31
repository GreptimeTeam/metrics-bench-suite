package sample_loader

import (
	"context"
	"encoding/base64"
	"fmt"
	"log"
	"math"
	"math/rand/v2"
	"metrics-bench-suite/pkg/http"
	"metrics-bench-suite/pkg/samples"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/prometheus/prompb"
	writev2 "github.com/prometheus/prometheus/prompb/io/prometheus/write/v2"
	"github.com/spf13/cobra"
)

type remoteWriteVersion uint8

const (
	remoteWriteV1 remoteWriteVersion = iota
	remoteWriteV2
)

// ponytail: fixed grace keeps shutdown bounded; add a flag only if receiver latency needs tuning.
const drainGracePeriod = 30 * time.Second

func parseRemoteWriteVersion(value string) (remoteWriteVersion, error) {
	switch value {
	case "v1":
		return remoteWriteV1, nil
	case "v2":
		return remoteWriteV2, nil
	default:
		return 0, fmt.Errorf("unsupported remote write version %q: expected v1 or v2", value)
	}
}

type runStats struct {
	requests                uint64
	samples                 uint64
	failedRequests          uint64
	samplesInFailedRequests uint64
	requestTimeTotal        time.Duration
	requestTimeMin          time.Duration
	requestTimeMax          time.Duration
}

func (s *runStats) record(samples uint64, failed bool, requestTime time.Duration) {
	if s == nil {
		return
	}
	s.requests++
	s.samples += samples
	s.requestTimeTotal += requestTime
	if s.requests == 1 || requestTime < s.requestTimeMin {
		s.requestTimeMin = requestTime
	}
	if requestTime > s.requestTimeMax {
		s.requestTimeMax = requestTime
	}
	if failed {
		s.failedRequests++
		s.samplesInFailedRequests += samples
	}
}

func logRunStats(statsByWorker []runStats, elapsed time.Duration, dryRun bool) {
	var total runStats
	for _, stats := range statsByWorker {
		total.requests += stats.requests
		total.samples += stats.samples
		total.failedRequests += stats.failedRequests
		total.samplesInFailedRequests += stats.samplesInFailedRequests
		if stats.requests == 0 {
			continue
		}
		total.requestTimeTotal += stats.requestTimeTotal
		if stats.requestTimeMin < total.requestTimeMin || total.requestTimeMin == 0 {
			total.requestTimeMin = stats.requestTimeMin
		}
		if stats.requestTimeMax > total.requestTimeMax {
			total.requestTimeMax = stats.requestTimeMax
		}
	}

	successfulRequests := total.requests - total.failedRequests
	samplesInSuccessfulRequests := total.samples - total.samplesInFailedRequests
	samplesPerSecond := 0.0
	if elapsed > 0 {
		samplesPerSecond = float64(samplesInSuccessfulRequests) / elapsed.Seconds()
	}

	rows := [][2]string{
		{"elapsed:", elapsed.Round(time.Millisecond).String()},
		{"requests_total:", strconv.FormatUint(total.requests, 10)},
		{"requests_succeeded:", strconv.FormatUint(successfulRequests, 10)},
		{"requests_failed:", strconv.FormatUint(total.failedRequests, 10)},
		{"samples_total:", strconv.FormatUint(total.samples, 10)},
		{"samples_in_succeeded_requests:", strconv.FormatUint(samplesInSuccessfulRequests, 10)},
		{"samples_in_failed_requests:", strconv.FormatUint(total.samplesInFailedRequests, 10)},
		{"samples_per_second:", fmt.Sprintf("%.2f", samplesPerSecond)},
	}
	if !dryRun && total.requests > 0 {
		requestTimeAvg := total.requestTimeTotal / time.Duration(total.requests)
		rows = append(rows,
			[2]string{"request_time_total:", total.requestTimeTotal.Round(time.Microsecond).String()},
			[2]string{"request_time_avg:", requestTimeAvg.Round(time.Microsecond).String()},
			[2]string{"request_time_min:", total.requestTimeMin.Round(time.Microsecond).String()},
			[2]string{"request_time_max:", total.requestTimeMax.Round(time.Microsecond).String()},
		)
	}
	rows = append(rows, [2]string{"dry_run:", strconv.FormatBool(dryRun)})

	width := 0
	for _, row := range rows {
		if len(row[0]) > width {
			width = len(row[0])
		}
	}

	var b strings.Builder
	b.WriteString("Run statistics:\n")
	for _, row := range rows {
		fmt.Fprintf(&b, "  %-*s %s\n", width, row[0], row[1])
	}
	log.Print(b.String())
}

func waitForWorkers(wg *sync.WaitGroup, cancel context.CancelFunc, timeout time.Duration) {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		log.Printf("Drain timeout reached, canceling unfinished requests")
		cancel()
		<-done
	}
}

// SampleLoader generates samples from config files and sends them to a remote-write endpoint.
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
	remoteWriteVersionValue, err := cmd.Flags().GetString("remote-write-version")
	if err != nil {
		return err
	}
	version, err := parseRemoteWriteVersion(remoteWriteVersionValue)
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
	duration, err := cmd.Flags().GetDuration("duration")
	if err != nil {
		return err
	}
	if cmd.Flags().Changed("duration") && duration <= 0 {
		return fmt.Errorf("duration must be greater than zero")
	}
	if s.Infinite && duration > 0 {
		return fmt.Errorf("duration and infinite cannot be used together")
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
	log.Printf("Remote write version: %s", remoteWriteVersionValue)
	log.Printf("Table pick rate: %d", s.TablePickCount)
	log.Printf("Replica label value: %d", s.Replica)
	log.Printf("Dry run: %t", s.DryRun)
	log.Printf("Duration: %s", duration)
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

	var statsByWorker []runStats
	if duration > 0 {
		statsByWorker = make([]runStats, s.Workers)
	}
	requestCtx, cancelRequests := context.WithCancel(context.Background())
	defer cancelRequests()
	wg := sync.WaitGroup{}
	for i := 0; i < s.Workers; i++ {
		var stats *runStats
		if statsByWorker != nil {
			stats = &statsByWorker[i]
		}
		wg.Add(1)
		go worker(requestCtx, i, s.RemoteWriteURL, s.authorizationHeader(), version, requestChan, &wg, s.DryRun, stats)
	}

	current := s.StartDate
	live := s.Infinite || duration > 0
	if live {
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

	generationCtx := context.Background()
	var durationStart time.Time
	if duration > 0 {
		durationStart = time.Now()
		var cancel context.CancelFunc
		generationCtx, cancel = context.WithTimeout(generationCtx, duration)
		defer cancel()
	}

	finish := func() error {
		close(requestChan)
		if duration > 0 {
			waitForWorkers(&wg, cancelRequests, drainGracePeriod)
			logRunStats(statsByWorker, time.Since(durationStart), s.DryRun)
		} else {
			wg.Wait()
		}
		return nil
	}

	// Track start time for churn epoch calculation
	churnEpochGenerator := samples.NewChurnEpochGenerator(s.ChurnInterval)
	currentEpoch := churnEpochGenerator.GetChurnEpoch()

	// First generation immediately after jitter
	log.Printf("Generating samples for %s (churn epoch: %d)", current, currentEpoch)
	if !s.convertToRemoteWriteRequestsStreaming(generationCtx, fileConfigs, current, requestChan, currentEpoch) {
		log.Printf("Duration reached, stopping")
		return finish()
	}
	current = current.Add(s.Interval)
	if !live {
		if current.After(s.EndDate) {
			log.Printf("End date reached, stopping")
			return finish()
		}
	}

	ticker := time.NewTicker(s.TickInterval)
	defer ticker.Stop()

runLoop:
	for {
		select {
		case <-ticker.C:
			newEpoch := churnEpochGenerator.GetChurnEpoch()
			if newEpoch != currentEpoch {
				currentEpoch = newEpoch
			}
			log.Printf("Generating samples for %s (churn epoch: %d)", current, currentEpoch)
			if !s.convertToRemoteWriteRequestsStreaming(generationCtx, fileConfigs, current, requestChan, currentEpoch) {
				log.Printf("Duration reached, stopping")
				break runLoop
			}
			current = current.Add(s.Interval)
			if !live && current.After(s.EndDate) {
				log.Printf("End date reached, stopping")
				break runLoop
			}
		case <-generationCtx.Done():
			log.Printf("Duration reached, stopping")
			break runLoop
		}
	}

	return finish()
}

func (s *SampleLoader) authorizationHeader() string {
	if s.Username == "" && s.Password == "" {
		return ""
	}
	authInfo := fmt.Sprintf("%s:%s", s.Username, s.Password)
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(authInfo))
}

func worker(ctx context.Context, id int, url string, authorizationHeader string, version remoteWriteVersion, request <-chan prompb.WriteRequest, wg *sync.WaitGroup, dryRun bool, stats *runStats) {
	defer wg.Done()
	for request := range request {
		if ctx.Err() != nil {
			return
		}
		numSeries := len(request.Timeseries)
		numSamples := uint64(0)
		for i := range request.Timeseries {
			numSamples += uint64(len(request.Timeseries[i].Samples))
		}
		failed := false
		var requestTime time.Duration
		if dryRun {
			log.Printf("worker %d (dry-run) would send request with num series: %d", id, numSeries)
		} else {
			now := time.Now()
			r := http.NewRequester(url)
			if authorizationHeader != "" {
				r.SetHeader("Authorization", authorizationHeader)
			}
			var err error
			switch version {
			case remoteWriteV1:
				err = r.SendContext(ctx, request)
			case remoteWriteV2:
				err = r.SendV2Context(ctx, convertToRemoteWriteV2Request(request))
			default:
				err = fmt.Errorf("unsupported remote write version: %d", version)
			}
			requestTime = time.Since(now)
			if err != nil {
				log.Printf("worker %d failed to send write request: %v", id, err)
				failed = true
			} else {
				log.Printf("worker %d sent request in %s, num series: %d", id, requestTime, numSeries)
			}
		}
		stats.record(numSamples, failed, requestTime)
	}
}

func convertToRemoteWriteV2Request(request prompb.WriteRequest) writev2.Request {
	symbols := writev2.NewSymbolTable()
	timeSeries := make([]writev2.TimeSeries, len(request.Timeseries))

	for i := range request.Timeseries {
		source := &request.Timeseries[i]
		labelRefs := make([]uint32, 0, len(source.Labels))
		for j := range source.Labels {
			labelRefs = append(labelRefs, symbols.Symbolize(source.Labels[j].Name))
			labelRefs = append(labelRefs, symbols.Symbolize(source.Labels[j].Value))
		}

		samples := make([]writev2.Sample, len(source.Samples))
		for j := range source.Samples {
			samples[j] = writev2.Sample{
				Value:     source.Samples[j].Value,
				Timestamp: source.Samples[j].Timestamp,
			}
		}

		timeSeries[i] = writev2.TimeSeries{
			LabelsRefs: labelRefs,
			Samples:    samples,
		}
	}

	return writev2.Request{
		Symbols:    symbols.Symbols(),
		Timeseries: timeSeries,
	}
}

// generateTimeSeriesForFileConfig generates time series for a single file config using a dedicated goroutine
func (s *SampleLoader) generateTimeSeriesForFileConfig(ctx context.Context, fileConfig *samples.FileConfig, currentTime time.Time, churnEpoch int64) <-chan prompb.TimeSeries {
	timeSeriesChan := make(chan prompb.TimeSeries, 1) // Buffered to allow the goroutine to start
	go func() {
		defer close(timeSeriesChan)
		fileConfig.GeneratePermutedTimeSeriesContext(ctx, currentTime, churnEpoch, s.Replica, timeSeriesChan)
	}()
	return timeSeriesChan
}

func (s *SampleLoader) convertToRemoteWriteRequestsStreaming(ctx context.Context, fileConfigs []samples.FileConfig, currentTime time.Time, requestChan chan<- prompb.WriteRequest, churnEpoch int64) bool {
	// Create a combined channel that merges all time series from all file configs
	timeSeriesChan := make(chan prompb.TimeSeries, len(fileConfigs))

	var wg sync.WaitGroup
	// Start a goroutine for each file config
	for i := range fileConfigs {
		wg.Add(1)
		go func(fc *samples.FileConfig) {
			defer wg.Done()
			// Get the time series channel for this file config
			tsChan := s.generateTimeSeriesForFileConfig(ctx, fc, currentTime, churnEpoch)
			// Forward all time series to the main channel
			for ts := range tsChan {
				select {
				case timeSeriesChan <- ts:
				case <-ctx.Done():
					return
				}
			}
		}(&fileConfigs[i])
	}

	// Close the main channel when all goroutines are done
	go func() {
		wg.Wait()
		close(timeSeriesChan)
	}()

	enqueue := func(timeSeries []prompb.TimeSeries) bool {
		select {
		case requestChan <- prompb.WriteRequest{Timeseries: timeSeries}:
			return true
		case <-ctx.Done():
			return false
		}
	}

	// Collect time series and send in batches
	tsSet := make([]prompb.TimeSeries, 0, s.MaxSamples)
	for ts := range timeSeriesChan {
		if ctx.Err() != nil {
			continue
		}
		tsSet = append(tsSet, ts)
		if len(tsSet) >= s.MaxSamples {
			// Send a batch when we reach maxSamples
			if !enqueue(tsSet) {
				for range timeSeriesChan {
				}
				return false
			}
			tsSet = make([]prompb.TimeSeries, 0, s.MaxSamples) // Reset the slice
		}
	}

	if ctx.Err() != nil {
		return false
	}

	// Send any remaining time series
	if len(tsSet) > 0 {
		return enqueue(tsSet)
	}
	return true
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
	rootCmd.Flags().String("remote-write-version", "v1", "The remote write protocol version (v1 or v2)")
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
	rootCmd.Flags().Duration("duration", 0, "Run from the current time for a finite duration (for example, 60s)")
	rootCmd.Flags().Uint64P("table-pick-count", "n", math.MaxUint64, "The number of tables to pick from")
	rootCmd.Flags().Bool("dry-run", false, "Run in dry-run mode without sending requests")
	rootCmd.Flags().Float64("churn-rate", 0.0, "The rate of time series to churn (0.0-1.0, e.g., 0.01 = 1%)")
	rootCmd.Flags().String("churn-interval", "0s", "The interval at which churn occurs (e.g., 10m)")

	return rootCmd
}
