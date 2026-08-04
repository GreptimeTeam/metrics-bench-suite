package sampleloader

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	benchhttp "metrics-bench-suite/pkg/http"
	"metrics-bench-suite/pkg/samples"

	"github.com/prometheus/prometheus/prompb"
	"github.com/spf13/cobra"
)

const (
	defaultBaselineDuration = 20 * time.Minute
	defaultBurstActive      = 20 * time.Minute
	defaultBurstPeriod      = time.Hour
	defaultObserveInterval  = 15 * time.Second
	workloadWindowInterval  = 10 * time.Second
)

type periodicOptions struct {
	BaselineDuration        time.Duration
	BurstActiveDuration     time.Duration
	BurstPeriod             time.Duration
	BurstCount              uint64
	ObserveInterval         time.Duration
	ObserveSQLURL           string
	TargetDatabase          string
	TargetPhysicalTable     string
	TargetLogicalTable      string
	AutopilotExpect         string
	PressureHighMinWriteBPS float64
	MonitoringURL           string
	MonitoringDatabase      string
	MonitoringTable         string
	MonitoringAuthorization string
	AutopilotConfigSnapshot string
	AutopilotConfigHash     string
}

func periodicOptionsFromCommand(cmd *cobra.Command, loader *SampleLoader) (periodicOptions, error) {
	parseDuration := func(name string) (time.Duration, error) {
		value, err := cmd.Flags().GetString(name)
		if err != nil {
			return 0, err
		}
		return time.ParseDuration(value)
	}

	options := periodicOptions{}
	var err error
	if options.BaselineDuration, err = parseDuration("baseline-duration"); err != nil {
		return periodicOptions{}, fmt.Errorf("parse baseline-duration: %w", err)
	}
	if options.BurstActiveDuration, err = parseDuration("burst-active-duration"); err != nil {
		return periodicOptions{}, fmt.Errorf("parse burst-active-duration: %w", err)
	}
	if options.BurstPeriod, err = parseDuration("burst-period"); err != nil {
		return periodicOptions{}, fmt.Errorf("parse burst-period: %w", err)
	}
	if options.ObserveInterval, err = parseDuration("observe-interval"); err != nil {
		return periodicOptions{}, fmt.Errorf("parse observe-interval: %w", err)
	}
	if options.BurstCount, err = cmd.Flags().GetUint64("burst-count"); err != nil {
		return periodicOptions{}, err
	}
	if options.ObserveSQLURL, err = cmd.Flags().GetString("observe-sql-url"); err != nil {
		return periodicOptions{}, err
	}
	if options.TargetDatabase, err = cmd.Flags().GetString("target-database"); err != nil {
		return periodicOptions{}, err
	}
	if options.TargetPhysicalTable, err = cmd.Flags().GetString("target-physical-table"); err != nil {
		return periodicOptions{}, err
	}
	if options.TargetLogicalTable, err = cmd.Flags().GetString("target-logical-table"); err != nil {
		return periodicOptions{}, err
	}
	if options.AutopilotExpect, err = cmd.Flags().GetString("autopilot-expect"); err != nil {
		return periodicOptions{}, err
	}
	if options.PressureHighMinWriteBPS, err = cmd.Flags().GetFloat64("pressure-high-min-write-bps"); err != nil {
		return periodicOptions{}, err
	}
	if options.MonitoringURL, err = cmd.Flags().GetString("monitoring-url"); err != nil {
		return periodicOptions{}, err
	}
	if options.MonitoringDatabase, err = cmd.Flags().GetString("monitoring-db"); err != nil {
		return periodicOptions{}, err
	}
	if options.MonitoringTable, err = cmd.Flags().GetString("monitoring-table"); err != nil {
		return periodicOptions{}, err
	}
	monitoringUser, err := cmd.Flags().GetString("monitoring-username")
	if err != nil {
		return periodicOptions{}, err
	}
	monitoringPassword, err := cmd.Flags().GetString("monitoring-password")
	if err != nil {
		return periodicOptions{}, err
	}
	if (monitoringUser == "") != (monitoringPassword == "") {
		return periodicOptions{}, fmt.Errorf("monitoring-username and monitoring-password must be provided together")
	}
	options.MonitoringAuthorization = loader.authorizationHeader()
	if monitoringUser != "" {
		options.MonitoringAuthorization = basicAuthorization(monitoringUser, monitoringPassword)
	}
	configFile, err := cmd.Flags().GetString("autopilot-config-file")
	if err != nil {
		return periodicOptions{}, err
	}
	if configFile != "" {
		data, err := os.ReadFile(configFile)
		if err != nil {
			return periodicOptions{}, fmt.Errorf("read autopilot-config-file: %w", err)
		}
		options.AutopilotConfigSnapshot = redactConfigSnapshot(string(data))
		digest := sha256.Sum256(data)
		options.AutopilotConfigHash = hex.EncodeToString(digest[:])
	}

	if options.BaselineDuration < 0 || options.BurstActiveDuration <= 0 || options.BurstPeriod < options.BurstActiveDuration || options.ObserveInterval <= 0 {
		return periodicOptions{}, fmt.Errorf("baseline-duration must not be negative; burst-active-duration and observe-interval must be positive; burst-period must be at least burst-active-duration")
	}
	if loader.TickInterval <= 0 || loader.Workers <= 0 {
		return periodicOptions{}, fmt.Errorf("tick-interval and workers must be greater than zero in periodic-burst mode")
	}
	if options.TargetDatabase == "" || options.TargetPhysicalTable == "" {
		return periodicOptions{}, fmt.Errorf("target-database and target-physical-table are required in periodic-burst mode")
	}
	if options.TargetLogicalTable == "" {
		options.TargetLogicalTable = options.TargetPhysicalTable
	}
	if !cmd.Flags().Changed("autopilot-expect") || (options.AutopilotExpect != "repartition" && options.AutopilotExpect != "rebalance" && options.AutopilotExpect != "both") {
		return periodicOptions{}, fmt.Errorf("autopilot-expect must be explicitly set to repartition, rebalance, or both in periodic-burst mode")
	}
	if options.PressureHighMinWriteBPS <= 0 {
		return periodicOptions{}, fmt.Errorf("pressure-high-min-write-bps must be greater than zero in periodic-burst mode")
	}
	if loader.DryRun {
		return options, nil
	}
	if options.MonitoringURL == "" {
		return periodicOptions{}, fmt.Errorf("monitoring-url is required when periodic-burst mode is not dry-run")
	}
	if options.ObserveSQLURL == "" {
		options.ObserveSQLURL, err = replaceEndpointPath(loader.RemoteWriteURL, "/v1/sql")
		if err != nil {
			return periodicOptions{}, fmt.Errorf("derive observe-sql-url: %w", err)
		}
	}
	if options.MonitoringURL, err = replaceEndpointPath(options.MonitoringURL, "/v1/ingest"); err != nil {
		return periodicOptions{}, fmt.Errorf("parse monitoring-url: %w", err)
	}
	return options, nil
}

func redactConfigSnapshot(snapshot string) string {
	lines := strings.Split(snapshot, "\n")
	for index, line := range lines {
		key, _, found := strings.Cut(line, ":")
		separator := ":"
		if !found {
			key, _, found = strings.Cut(line, "=")
			separator = "="
		}
		if !found {
			continue
		}
		normalizedKey := strings.ToLower(strings.TrimSpace(key))
		if strings.Contains(normalizedKey, "password") || strings.Contains(normalizedKey, "secret") || strings.Contains(normalizedKey, "token") || strings.Contains(normalizedKey, "private_key") {
			lines[index] = key + separator + " <redacted>"
		}
	}
	return strings.Join(lines, "\n")
}

func basicAuthorization(username, password string) string {
	return "Basic " + base64Encode(username+":"+password)
}

func base64Encode(value string) string {
	return base64.StdEncoding.EncodeToString([]byte(value))
}

func replaceEndpointPath(value, path string) (string, error) {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		if err == nil {
			err = fmt.Errorf("URL must include scheme and host")
		}
		return "", err
	}
	parsed.Path = path
	parsed.RawQuery = ""
	return parsed.String(), nil
}

type benchmarkEvent struct {
	EventTSMS                 int64   `json:"event_ts_ms"`
	EventSequence             uint64  `json:"event_sequence"`
	RunID                     string  `json:"run_id"`
	EventType                 string  `json:"event_type"`
	Phase                     string  `json:"phase"`
	Cycle                     uint64  `json:"cycle"`
	TargetDatabase            string  `json:"target_database"`
	LogicalTable              string  `json:"logical_table"`
	PhysicalTable             string  `json:"physical_table"`
	TableID                   uint64  `json:"table_id"`
	RegionID                  uint64  `json:"region_id"`
	RegionNumber              uint64  `json:"region_number"`
	Partition                 string  `json:"partition"`
	LeaderDatanode            string  `json:"leader_datanode"`
	RegionRole                string  `json:"region_role"`
	Engine                    string  `json:"engine"`
	WindowStartMS             int64   `json:"window_start_ms"`
	WindowEndMS               int64   `json:"window_end_ms"`
	ScheduledTSMS             int64   `json:"scheduled_ts_ms"`
	ActualTSMS                int64   `json:"actual_ts_ms"`
	ScheduleDelayMS           int64   `json:"schedule_delay_ms"`
	RequestCount              uint64  `json:"request_count"`
	SampleCount               uint64  `json:"sample_count"`
	PayloadBytes              uint64  `json:"payload_bytes"`
	SuccessCount              uint64  `json:"success_count"`
	ErrorCount                uint64  `json:"error_count"`
	WriteBPS                  float64 `json:"write_bps"`
	LatencyP50MS              float64 `json:"latency_p50_ms"`
	LatencyP95MS              float64 `json:"latency_p95_ms"`
	LatencyMaxMS              float64 `json:"latency_max_ms"`
	PressureThresholdBPS      float64 `json:"pressure_threshold_bps"`
	ConsecutiveHighWindows    uint64  `json:"consecutive_high_windows"`
	RegionRows                uint64  `json:"region_rows"`
	WrittenBytesSinceOpen     uint64  `json:"written_bytes_since_open"`
	QueryCPUTimeMillis        uint64  `json:"query_cpu_time_millis"`
	QueryScannedBytes         uint64  `json:"query_scanned_bytes"`
	DiskSize                  uint64  `json:"disk_size"`
	MemtableSize              uint64  `json:"memtable_size"`
	ManifestSize              uint64  `json:"manifest_size"`
	SSTSize                   uint64  `json:"sst_size"`
	SSTNum                    uint64  `json:"sst_num"`
	IndexSize                 uint64  `json:"index_size"`
	IntervalMS                int64   `json:"interval_ms"`
	MemtableSizeDeltaBytes    int64   `json:"memtable_size_delta_bytes"`
	MemtableWriteBPSApprox    float64 `json:"memtable_write_bps_approx"`
	WrittenBytesBPS           float64 `json:"written_bytes_bps"`
	RegionNew                 bool    `json:"region_new"`
	CounterReset              bool    `json:"counter_reset"`
	MemtableDecreased         bool    `json:"memtable_decreased"`
	AnalysisContextIncomplete bool    `json:"analysis_context_incomplete"`
	AutopilotExpect           string  `json:"autopilot_expect"`
	AutopilotConfigHash       string  `json:"autopilot_config_hash"`
	AutopilotConfigSnapshot   string  `json:"autopilot_config_snapshot"`
	Error                     string  `json:"error"`
	Details                   string  `json:"details"`
}

type eventWriter struct {
	client        *http.Client
	endpoint      string
	database      string
	table         string
	authorization string
	mu            sync.Mutex
	sequence      uint64
	pending       []benchmarkEvent
	firstErr      error
}

func newEventWriter(options periodicOptions) *eventWriter {
	return &eventWriter{client: &http.Client{Timeout: 30 * time.Second}, endpoint: options.MonitoringURL, database: options.MonitoringDatabase, table: options.MonitoringTable, authorization: options.MonitoringAuthorization}
}

func (w *eventWriter) emit(ctx context.Context, event benchmarkEvent) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.sequence++
	event.EventSequence = w.sequence
	if event.EventTSMS == 0 {
		event.EventTSMS = time.Now().UnixMilli()
	}
	w.pending = append(w.pending, event)
	if len(w.pending) >= 100 {
		w.flushLocked(ctx)
	}
}

func (w *eventWriter) close(ctx context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.flushLocked(ctx)
	return w.firstErr
}

func (w *eventWriter) flushLocked(ctx context.Context) {
	if len(w.pending) == 0 {
		return
	}
	var body bytes.Buffer
	encoder := json.NewEncoder(&body)
	for _, event := range w.pending {
		if err := encoder.Encode(event); err != nil {
			w.recordError(err)
			w.pending = nil
			return
		}
	}
	endpoint, err := url.Parse(w.endpoint)
	if err != nil {
		w.recordError(err)
		w.pending = nil
		return
	}
	query := endpoint.Query()
	query.Set("db", w.database)
	query.Set("table", w.table)
	query.Set("pipeline_name", "greptime_identity")
	query.Set("custom_time_index", "event_ts_ms;epoch;ms")
	endpoint.RawQuery = query.Encode()

	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body.Bytes()))
		if err == nil {
			req.Header.Set("Content-Type", "application/x-ndjson")
			if w.authorization != "" {
				req.Header.Set("Authorization", w.authorization)
			}
			resp, doErr := w.client.Do(req)
			if doErr == nil {
				responseBody, readErr := io.ReadAll(resp.Body)
				resp.Body.Close()
				if readErr == nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
					w.pending = nil
					return
				}
				if readErr != nil {
					lastErr = readErr
				} else {
					lastErr = fmt.Errorf("ingest benchmark events: %s: %s", resp.Status, string(responseBody))
				}
			} else {
				lastErr = doErr
			}
		} else {
			lastErr = err
		}
		time.Sleep(time.Duration(attempt+1) * 200 * time.Millisecond)
	}
	w.recordError(lastErr)
	w.pending = nil
}

func (w *eventWriter) recordError(err error) {
	if err != nil && w.firstErr == nil {
		w.firstErr = err
	}
}

type writeResult struct {
	duration     time.Duration
	requestCount uint64
	sampleCount  uint64
	payloadBytes uint64
	err          error
}

func periodicWorker(url, authorization string, requests <-chan prompb.WriteRequest, results chan<- writeResult, wg *sync.WaitGroup, dryRun bool) {
	defer wg.Done()
	for request := range requests {
		result := writeResult{requestCount: 1, sampleCount: sampleCount(request)}
		started := time.Now()
		if dryRun {
			result.payloadBytes = 0
		} else {
			requester := benchhttp.NewRequester(url)
			if authorization != "" {
				requester.SetHeader("Authorization", authorization)
			}
			stats, err := requester.SendWithStats(request)
			result.payloadBytes = uint64(stats.PayloadBytes)
			result.err = err
		}
		result.duration = time.Since(started)
		results <- result
	}
}

func sampleCount(request prompb.WriteRequest) uint64 {
	var count uint64
	for _, series := range request.Timeseries {
		count += uint64(len(series.Samples))
	}
	return count
}

type workloadMetrics struct {
	requestCount uint64
	sampleCount  uint64
	payloadBytes uint64
	successCount uint64
	errorCount   uint64
	latencies    []time.Duration
	lastError    string
}

type workloadCollector struct {
	mu      sync.Mutex
	metrics workloadMetrics
}

func (c *workloadCollector) add(result writeResult) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.metrics.requestCount += result.requestCount
	c.metrics.sampleCount += result.sampleCount
	c.metrics.payloadBytes += result.payloadBytes
	if result.err != nil {
		c.metrics.errorCount++
		c.metrics.lastError = result.err.Error()
	} else {
		c.metrics.successCount++
	}
	c.metrics.latencies = append(c.metrics.latencies, result.duration)
}

func (c *workloadCollector) reset() workloadMetrics {
	c.mu.Lock()
	defer c.mu.Unlock()
	metrics := c.metrics
	c.metrics = workloadMetrics{}
	return metrics
}

func (s *SampleLoader) runPeriodic(cmd *cobra.Command, fileConfigs []samples.FileConfig) (runErr error) {
	options, err := periodicOptionsFromCommand(cmd, s)
	if err != nil {
		return err
	}
	if s.DryRun {
		log.Printf("periodic-burst dry-run enabled; monitoring and region observations are disabled")
	}

	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	runID := newRunID()
	baseEvent := benchmarkEvent{RunID: runID, TargetDatabase: options.TargetDatabase, LogicalTable: options.TargetLogicalTable, PhysicalTable: options.TargetPhysicalTable, AutopilotExpect: options.AutopilotExpect, AutopilotConfigHash: options.AutopilotConfigHash, AutopilotConfigSnapshot: options.AutopilotConfigSnapshot, AnalysisContextIncomplete: options.AutopilotConfigSnapshot == ""}

	var writer *eventWriter
	if !s.DryRun {
		writer = newEventWriter(options)
		writer.emit(ctx, eventWith(baseEvent, "run_started", "baseline", 0, func(event *benchmarkEvent) {
			event.Details = fmt.Sprintf(`{"baseline_duration":"%s","burst_active_duration":"%s","burst_period":"%s","burst_count":%d,"observe_interval":"%s","pressure_high_min_write_bps":%g}`, options.BaselineDuration, options.BurstActiveDuration, options.BurstPeriod, options.BurstCount, options.ObserveInterval, options.PressureHighMinWriteBPS)
		}))
	}

	observerContext, cancelObserver := context.WithCancel(ctx)
	var observerDone chan struct{}
	if writer != nil {
		observerDone = make(chan struct{})
		go func() {
			defer close(observerDone)
			s.observeRegions(observerContext, options, writer, baseEvent)
		}()
	}
	observerStopped := false
	defer func() {
		if observerStopped {
			// The normal completion path already joined the observer.
		} else {
			cancelObserver()
			if observerDone != nil {
				<-observerDone
			}
		}
		if writer == nil {
			return
		}
		finishContext := context.Background()
		if runErr != nil {
			writer.emit(finishContext, eventWith(baseEvent, "run_failed", "failed", 0, func(event *benchmarkEvent) {
				event.Error = runErr.Error()
			}))
		}
		if closeErr := writer.close(finishContext); runErr == nil && closeErr != nil {
			runErr = fmt.Errorf("flush benchmark events: %w", closeErr)
		}
	}()

	if err := waitContext(ctx, options.BaselineDuration); err != nil {
		return err
	}

	current := time.Now()
	churnEpochGenerator := samples.NewChurnEpochGenerator(s.ChurnInterval)
	for cycle := uint64(1); options.BurstCount == 0 || cycle <= options.BurstCount; cycle++ {
		if writer != nil {
			writer.emit(ctx, eventWith(baseEvent, "pressure_started", "active", cycle, func(event *benchmarkEvent) {
				now := time.Now()
				event.ScheduledTSMS = now.UnixMilli()
				event.ActualTSMS = now.UnixMilli()
				event.PressureThresholdBPS = options.PressureHighMinWriteBPS
			}))
		}
		if err := s.runPeriodicActive(ctx, fileConfigs, &current, churnEpochGenerator, options, writer, baseEvent, cycle); err != nil {
			return err
		}
		cooldown := options.BurstPeriod - options.BurstActiveDuration
		if writer != nil {
			writer.emit(ctx, eventWith(baseEvent, "phase_completed", "active", cycle, func(event *benchmarkEvent) {
				event.Details = "active requests drained"
			}))
		}
		if err := waitContext(ctx, cooldown); err != nil {
			return err
		}
		if writer != nil {
			writer.emit(ctx, eventWith(baseEvent, "phase_completed", "cooldown", cycle, nil))
		}
	}

	cancelObserver()
	if observerDone != nil {
		<-observerDone
	}
	observerStopped = true
	if writer == nil {
		return nil
	}
	writer.emit(ctx, eventWith(baseEvent, "run_completed", "completed", 0, nil))
	return nil
}

func (s *SampleLoader) runPeriodicActive(ctx context.Context, fileConfigs []samples.FileConfig, current *time.Time, churn *samples.ChurnEpochGenerator, options periodicOptions, writer *eventWriter, base benchmarkEvent, cycle uint64) error {
	requests := make(chan prompb.WriteRequest, s.Workers)
	results := make(chan writeResult, s.Workers*2)
	collector := &workloadCollector{}
	collectorDone := make(chan struct{})
	go func() {
		defer close(collectorDone)
		for result := range results {
			collector.add(result)
		}
	}()

	var workers sync.WaitGroup
	for i := 0; i < s.Workers; i++ {
		workers.Add(1)
		go periodicWorker(s.RemoteWriteURL, s.authorizationHeader(), requests, results, &workers, s.DryRun)
	}

	started := time.Now()
	deadline := started.Add(options.BurstActiveDuration)
	tick := time.NewTicker(s.TickInterval)
	window := time.NewTicker(workloadWindowInterval)
	defer tick.Stop()
	defer window.Stop()
	windowStarted := started
	consecutiveHigh := uint64(0)
	generate := func() {
		epoch := churn.GetChurnEpoch()
		s.convertToRemoteWriteRequestsStreaming(fileConfigs, *current, requests, epoch)
		*current = current.Add(s.Interval)
	}
	generate()

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			close(requests)
			workers.Wait()
			close(results)
			<-collectorDone
			return ctx.Err()
		case <-tick.C:
			generate()
		case now := <-window.C:
			consecutiveHigh = emitWorkloadWindow(ctx, writer, base, cycle, windowStarted, now, collector.reset(), options.PressureHighMinWriteBPS, consecutiveHigh)
			windowStarted = now
		}
	}
	close(requests)
	workers.Wait()
	close(results)
	<-collectorDone
	now := time.Now()
	emitWorkloadWindow(ctx, writer, base, cycle, windowStarted, now, collector.reset(), options.PressureHighMinWriteBPS, consecutiveHigh)
	return nil
}

func emitWorkloadWindow(ctx context.Context, writer *eventWriter, base benchmarkEvent, cycle uint64, start, end time.Time, metrics workloadMetrics, threshold float64, consecutiveHigh uint64) uint64 {
	if writer == nil || !end.After(start) {
		return consecutiveHigh
	}
	latencies := append([]time.Duration(nil), metrics.latencies...)
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	duration := end.Sub(start)
	writeBPS := float64(metrics.payloadBytes) / duration.Seconds()
	event := eventWith(base, "workload_window", "active", cycle, func(event *benchmarkEvent) {
		event.WindowStartMS = start.UnixMilli()
		event.WindowEndMS = end.UnixMilli()
		event.RequestCount = metrics.requestCount
		event.SampleCount = metrics.sampleCount
		event.PayloadBytes = metrics.payloadBytes
		event.SuccessCount = metrics.successCount
		event.ErrorCount = metrics.errorCount
		event.WriteBPS = writeBPS
		event.LatencyP50MS = latencyPercentileMS(latencies, 0.50)
		event.LatencyP95MS = latencyPercentileMS(latencies, 0.95)
		event.LatencyMaxMS = latencyPercentileMS(latencies, 1)
		event.PressureThresholdBPS = threshold
	})
	if writeBPS >= threshold && metrics.errorCount == 0 {
		consecutiveHigh++
	} else {
		if consecutiveHigh >= 3 {
			writer.emit(ctx, eventWith(base, "pressure_dropped", "active", cycle, func(event *benchmarkEvent) {
				event.WriteBPS = writeBPS
				event.PressureThresholdBPS = threshold
				event.ConsecutiveHighWindows = consecutiveHigh
			}))
		}
		consecutiveHigh = 0
	}
	event.ConsecutiveHighWindows = consecutiveHigh
	writer.emit(ctx, event)
	if metrics.errorCount > 0 {
		writer.emit(ctx, eventWith(base, "write_error", "active", cycle, func(event *benchmarkEvent) {
			event.WindowStartMS = start.UnixMilli()
			event.WindowEndMS = end.UnixMilli()
			event.ErrorCount = metrics.errorCount
			event.Error = metrics.lastError
		}))
	}
	if consecutiveHigh == 3 {
		writer.emit(ctx, eventWith(base, "pressure_observed_high", "active", cycle, func(event *benchmarkEvent) {
			event.WriteBPS = writeBPS
			event.PressureThresholdBPS = threshold
			event.ConsecutiveHighWindows = consecutiveHigh
		}))
	}
	return consecutiveHigh
}

func latencyPercentileMS(values []time.Duration, percentile float64) float64 {
	if len(values) == 0 {
		return 0
	}
	index := int(math.Ceil(float64(len(values))*percentile)) - 1
	if index < 0 {
		index = 0
	}
	return float64(values[index]) / float64(time.Millisecond)
}

func eventWith(base benchmarkEvent, eventType, phase string, cycle uint64, update func(*benchmarkEvent)) benchmarkEvent {
	event := base
	event.EventType = eventType
	event.Phase = phase
	event.Cycle = cycle
	if update != nil {
		update(&event)
	}
	return event
}

func waitContext(ctx context.Context, duration time.Duration) error {
	if duration == 0 {
		return nil
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func newRunID() string {
	bytes := make([]byte, 8)
	if _, err := rand.Read(bytes); err != nil {
		return fmt.Sprintf("run-%d", time.Now().UnixNano())
	}
	return fmt.Sprintf("run-%d-%x", time.Now().UnixMilli(), bytes)
}

type regionSnapshot struct {
	timestamp             time.Time
	tableID               uint64
	regionID              uint64
	regionNumber          uint64
	partition             string
	leader                string
	regionRows            uint64
	writtenBytesSinceOpen uint64
	queryCPUTimeMillis    uint64
	queryScannedBytes     uint64
	diskSize              uint64
	memtableSize          uint64
	manifestSize          uint64
	sstSize               uint64
	sstNum                uint64
	indexSize             uint64
	engine                string
	role                  string
}

func (s *SampleLoader) observeRegions(ctx context.Context, options periodicOptions, writer *eventWriter, base benchmarkEvent) {
	client := httpSQLClient{endpoint: options.ObserveSQLURL, database: options.TargetDatabase, authorization: s.authorizationHeader(), client: &http.Client{Timeout: 30 * time.Second}}
	previous := make(map[uint64]regionSnapshot)
	observe := func() {
		now := time.Now()
		current, err := client.regionSnapshots(ctx, options.TargetPhysicalTable, now)
		if err != nil {
			writer.emit(ctx, eventWith(base, "observation_error", "observe", 0, func(event *benchmarkEvent) { event.Error = err.Error() }))
			return
		}
		seen := make(map[uint64]struct{}, len(current))
		for _, snapshot := range current {
			seen[snapshot.regionID] = struct{}{}
			prior, exists := previous[snapshot.regionID]
			event := regionEvent(base, snapshot, exists, prior)
			writer.emit(ctx, event)
			if !exists {
				writer.emit(ctx, eventWith(event, "region_created", "observe", 0, nil))
			} else if prior.leader != snapshot.leader {
				writer.emit(ctx, eventWith(event, "leader_changed", "observe", 0, nil))
			}
		}
		for id, snapshot := range previous {
			if _, ok := seen[id]; !ok {
				writer.emit(ctx, eventWith(regionEvent(base, snapshot, true, snapshot), "region_disappeared", "observe", 0, nil))
			}
		}
		previous = make(map[uint64]regionSnapshot, len(current))
		for _, snapshot := range current {
			previous[snapshot.regionID] = snapshot
		}
	}

	observe()
	ticker := time.NewTicker(options.ObserveInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			observe()
		}
	}
}

func regionEvent(base benchmarkEvent, snapshot regionSnapshot, exists bool, previous regionSnapshot) benchmarkEvent {
	event := eventWith(base, "region_stats_snapshot", "observe", 0, func(event *benchmarkEvent) {
		event.EventTSMS = snapshot.timestamp.UnixMilli()
		event.TableID = snapshot.tableID
		event.RegionID = snapshot.regionID
		event.RegionNumber = snapshot.regionNumber
		event.Partition = snapshot.partition
		event.LeaderDatanode = snapshot.leader
		event.RegionRole = snapshot.role
		event.Engine = snapshot.engine
		event.RegionRows = snapshot.regionRows
		event.WrittenBytesSinceOpen = snapshot.writtenBytesSinceOpen
		event.QueryCPUTimeMillis = snapshot.queryCPUTimeMillis
		event.QueryScannedBytes = snapshot.queryScannedBytes
		event.DiskSize = snapshot.diskSize
		event.MemtableSize = snapshot.memtableSize
		event.ManifestSize = snapshot.manifestSize
		event.SSTSize = snapshot.sstSize
		event.SSTNum = snapshot.sstNum
		event.IndexSize = snapshot.indexSize
		event.RegionNew = !exists
	})
	if !exists {
		return event
	}
	elapsed := snapshot.timestamp.Sub(previous.timestamp)
	if elapsed <= 0 {
		return event
	}
	event.IntervalMS = elapsed.Milliseconds()
	event.MemtableSizeDeltaBytes = int64(snapshot.memtableSize) - int64(previous.memtableSize)
	event.MemtableDecreased = snapshot.memtableSize < previous.memtableSize
	event.CounterReset = snapshot.writtenBytesSinceOpen < previous.writtenBytesSinceOpen
	if !event.CounterReset {
		event.WrittenBytesBPS = float64(snapshot.writtenBytesSinceOpen-previous.writtenBytesSinceOpen) / elapsed.Seconds()
	}
	if event.MemtableSizeDeltaBytes >= 0 {
		event.MemtableWriteBPSApprox = float64(event.MemtableSizeDeltaBytes) / elapsed.Seconds()
	}
	return event
}

type httpSQLClient struct {
	endpoint      string
	database      string
	authorization string
	client        *http.Client
}

func (c httpSQLClient) regionSnapshots(ctx context.Context, physicalTable string, now time.Time) ([]regionSnapshot, error) {
	sql := `SELECT p.partition_name, p.greptime_partition_id, rp.peer_id, s.table_id, s.region_number, s.region_rows, s.written_bytes_since_open, s.query_cpu_time_millis, s.query_scanned_bytes, s.disk_size, s.memtable_size, s.manifest_size, s.sst_size, s.sst_num, s.index_size, s.engine, s.region_role FROM information_schema.partitions AS p LEFT JOIN information_schema.region_statistics AS s ON p.greptime_partition_id = s.region_id LEFT JOIN information_schema.region_peers AS rp ON s.region_id = rp.region_id AND rp.is_leader = 'Yes' WHERE p.table_schema = ` + quoteSQLLiteral(c.database) + ` AND p.table_name = ` + quoteSQLLiteral(physicalTable) + ` ORDER BY p.partition_ordinal_position`
	rows, err := c.query(ctx, sql)
	if err != nil {
		return nil, err
	}
	result := make([]regionSnapshot, 0, len(rows))
	for _, row := range rows {
		regionID := uintValue(row["greptime_partition_id"])
		if regionID == 0 {
			continue
		}
		result = append(result, regionSnapshot{timestamp: now, partition: stringValue(row["partition_name"]), regionID: regionID, leader: stringValue(row["peer_id"]), tableID: uintValue(row["table_id"]), regionNumber: uintValue(row["region_number"]), regionRows: uintValue(row["region_rows"]), writtenBytesSinceOpen: uintValue(row["written_bytes_since_open"]), queryCPUTimeMillis: uintValue(row["query_cpu_time_millis"]), queryScannedBytes: uintValue(row["query_scanned_bytes"]), diskSize: uintValue(row["disk_size"]), memtableSize: uintValue(row["memtable_size"]), manifestSize: uintValue(row["manifest_size"]), sstSize: uintValue(row["sst_size"]), sstNum: uintValue(row["sst_num"]), indexSize: uintValue(row["index_size"]), engine: stringValue(row["engine"]), role: stringValue(row["region_role"])})
	}
	return result, nil
}

func (c httpSQLClient) query(ctx context.Context, sql string) ([]map[string]any, error) {
	endpoint, err := url.Parse(c.endpoint)
	if err != nil {
		return nil, err
	}
	query := endpoint.Query()
	query.Set("db", c.database)
	endpoint.RawQuery = query.Encode()
	form := url.Values{"sql": []string{sql}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if c.authorization != "" {
		req.Header.Set("Authorization", c.authorization)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP SQL: %s: %s", resp.Status, string(body))
	}
	return decodeSQLRows(body)
}

func decodeSQLRows(body []byte) ([]map[string]any, error) {
	var response struct {
		Output []struct {
			Records struct {
				Schema struct {
					ColumnSchemas []struct {
						Name string `json:"name"`
					} `json:"column_schemas"`
				} `json:"schema"`
				Rows [][]any `json:"rows"`
			} `json:"records"`
		} `json:"output"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("decode HTTP SQL response: %w", err)
	}
	if len(response.Output) == 0 {
		return nil, nil
	}
	records := response.Output[0].Records
	rows := make([]map[string]any, 0, len(records.Rows))
	for _, values := range records.Rows {
		row := make(map[string]any, len(records.Schema.ColumnSchemas))
		for index, column := range records.Schema.ColumnSchemas {
			if index < len(values) {
				row[column.Name] = values[index]
			}
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func quoteSQLLiteral(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

func uintValue(value any) uint64 {
	switch value := value.(type) {
	case float64:
		if value >= 0 {
			return uint64(value)
		}
	case json.Number:
		parsed, _ := strconv.ParseUint(value.String(), 10, 64)
		return parsed
	case string:
		parsed, _ := strconv.ParseUint(value, 10, 64)
		return parsed
	}
	return 0
}

func stringValue(value any) string {
	if value == nil {
		return ""
	}
	if stringValue, ok := value.(string); ok {
		return stringValue
	}
	return fmt.Sprint(value)
}
