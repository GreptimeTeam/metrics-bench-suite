package sampleloader

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	mathrand "math/rand"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	benchhttp "metrics-bench-suite/pkg/http"
	sharedpartition "metrics-bench-suite/pkg/partition"
	"metrics-bench-suite/pkg/samples"

	"github.com/prometheus/prometheus/prompb"
	"github.com/spf13/cobra"
)

const (
	defaultBaselineDuration = 20 * time.Minute
	defaultBaselineTick     = time.Minute
	defaultBurstActive      = 20 * time.Minute
	defaultBurstPeriod      = time.Hour
	defaultBurstGap         = 15 * time.Minute
	defaultBurstJitter      = 0.2
	defaultObserveInterval  = 15 * time.Second
	defaultTransientBurst   = 5 * time.Minute
	workloadWindowInterval  = 10 * time.Second
)

type periodicOptions struct {
	BaselineDuration        time.Duration
	BaselineTickInterval    time.Duration
	BurstActiveDuration     time.Duration
	TransientBurstDuration  time.Duration
	BurstPeriod             time.Duration
	BurstGap                time.Duration
	BurstJitter             float64
	BurstAmplification      uint64
	BurstCount              uint64
	RandomSeed              int64
	ObserveInterval         time.Duration
	ObserveSQLURL           string
	TargetDatabase          string
	TargetPhysicalTable     string
	AutopilotExpect         string
	PressureHighMinWriteBPS float64
	TrafficMode             string
	BurstClass              burstClass
	BaselineTargetWriteBPS  float64
	QualifiedMaxWriteBPS    float64
	MonitoringURL           string
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
	if options.BaselineTickInterval, err = parseDuration("baseline-tick-interval"); err != nil {
		return periodicOptions{}, fmt.Errorf("parse baseline-tick-interval: %w", err)
	}
	if options.BurstActiveDuration, err = parseDuration("burst-active-duration"); err != nil {
		return periodicOptions{}, fmt.Errorf("parse burst-active-duration: %w", err)
	}
	if options.TransientBurstDuration, err = parseDuration("transient-burst-duration"); err != nil {
		return periodicOptions{}, fmt.Errorf("parse transient-burst-duration: %w", err)
	}
	if options.BurstPeriod, err = parseDuration("burst-period"); err != nil {
		return periodicOptions{}, fmt.Errorf("parse burst-period: %w", err)
	}
	if options.BurstGap, err = parseDuration("burst-gap"); err != nil {
		return periodicOptions{}, fmt.Errorf("parse burst-gap: %w", err)
	}
	if options.BurstJitter, err = cmd.Flags().GetFloat64("burst-jitter"); err != nil {
		return periodicOptions{}, err
	}
	if options.BurstAmplification, err = cmd.Flags().GetUint64("burst-amplification"); err != nil {
		return periodicOptions{}, err
	}
	if options.ObserveInterval, err = parseDuration("observe-interval"); err != nil {
		return periodicOptions{}, fmt.Errorf("parse observe-interval: %w", err)
	}
	if options.BurstCount, err = cmd.Flags().GetUint64("burst-count"); err != nil {
		return periodicOptions{}, err
	}
	if options.RandomSeed, err = cmd.Flags().GetInt64("random-seed"); err != nil {
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
	if options.AutopilotExpect, err = cmd.Flags().GetString("autopilot-expect"); err != nil {
		return periodicOptions{}, err
	}
	if options.PressureHighMinWriteBPS, err = cmd.Flags().GetFloat64("pressure-high-min-write-bps"); err != nil {
		return periodicOptions{}, err
	}
	if options.TrafficMode, err = cmd.Flags().GetString("periodic-traffic-mode"); err != nil {
		return periodicOptions{}, err
	}
	burstClassValue, err := cmd.Flags().GetString("burst-class")
	if err != nil {
		return periodicOptions{}, err
	}
	if options.BaselineTargetWriteBPS, err = cmd.Flags().GetFloat64("baseline-target-write-bps"); err != nil {
		return periodicOptions{}, err
	}
	if options.QualifiedMaxWriteBPS, err = cmd.Flags().GetFloat64("qualified-max-write-bps"); err != nil {
		return periodicOptions{}, err
	}
	if options.MonitoringURL, err = cmd.Flags().GetString("self-monitoring-url"); err != nil {
		return periodicOptions{}, err
	}
	options.MonitoringAuthorization = loader.authorizationHeader()
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

	if options.BaselineDuration < 0 || options.BaselineTickInterval <= 0 || options.BurstActiveDuration <= 0 || options.TransientBurstDuration <= 0 || options.BurstGap < 0 || options.ObserveInterval <= 0 {
		return periodicOptions{}, fmt.Errorf("baseline-duration and burst-gap must not be negative; baseline-tick-interval, burst-active-duration, transient-burst-duration, and observe-interval must be positive")
	}
	if options.BurstJitter < 0 || options.BurstJitter > 1 {
		return periodicOptions{}, fmt.Errorf("burst-jitter must be between 0 and 1")
	}
	if options.BurstAmplification == 0 {
		return periodicOptions{}, fmt.Errorf("burst-amplification must be greater than 0")
	}
	if loader.TickInterval <= 0 || loader.Workers <= 0 {
		return periodicOptions{}, fmt.Errorf("tick-interval and workers must be greater than zero in periodic-burst mode")
	}
	if options.TargetDatabase == "" || options.TargetPhysicalTable == "" {
		return periodicOptions{}, fmt.Errorf("target-database and target-physical-table are required in periodic-burst mode")
	}
	if !cmd.Flags().Changed("autopilot-expect") || (options.AutopilotExpect != "repartition" && options.AutopilotExpect != "rebalance" && options.AutopilotExpect != "both") {
		return periodicOptions{}, fmt.Errorf("autopilot-expect must be explicitly set to repartition, rebalance, or both in periodic-burst mode")
	}
	if options.PressureHighMinWriteBPS <= 0 {
		return periodicOptions{}, fmt.Errorf("pressure-high-min-write-bps must be greater than zero in periodic-burst mode")
	}
	if options.TrafficMode != "legacy" && options.TrafficMode != "steady" {
		return periodicOptions{}, fmt.Errorf("periodic-traffic-mode must be legacy or steady")
	}
	if options.TrafficMode == "legacy" && (options.BurstPeriod < options.BurstActiveDuration || options.BurstPeriod < options.TransientBurstDuration) {
		return periodicOptions{}, fmt.Errorf("burst-period must be at least each burst duration in legacy mode")
	}
	if burstClassValue == "" {
		if options.TrafficMode == "steady" {
			options.BurstClass = burstClassMixed
		} else {
			options.BurstClass = burstClassQualified
		}
	} else {
		options.BurstClass = burstClass(burstClassValue)
	}
	if !options.BurstClass.valid() {
		return periodicOptions{}, fmt.Errorf("burst-class must be mixed, transient, or qualified")
	}
	if options.BaselineTargetWriteBPS < 0 || options.QualifiedMaxWriteBPS < 0 {
		return periodicOptions{}, fmt.Errorf("baseline-target-write-bps and qualified-max-write-bps must not be negative")
	}
	if options.BaselineTargetWriteBPS == 0 {
		options.BaselineTargetWriteBPS = options.PressureHighMinWriteBPS * 0.25
	}
	if options.QualifiedMaxWriteBPS == 0 {
		options.QualifiedMaxWriteBPS = options.PressureHighMinWriteBPS * 3
	}
	if loader.DryRun {
		return options, nil
	}
	if options.MonitoringURL == "" {
		return periodicOptions{}, fmt.Errorf("self-monitoring-url is required when periodic-burst mode is not dry-run")
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

type burstClass string

const (
	burstClassMixed     burstClass = "mixed"
	burstClassTransient burstClass = "transient"
	burstClassQualified burstClass = "qualified"
)

func (c burstClass) valid() bool {
	return c == burstClassMixed || c == burstClassTransient || c == burstClassQualified
}

func (c burstClass) expectation(configured string) string {
	if c == burstClassTransient {
		return "none"
	}
	return configured
}

func (c burstClass) duration(options periodicOptions) time.Duration {
	if c == burstClassTransient {
		return options.TransientBurstDuration
	}
	return options.BurstActiveDuration
}

func (c burstClass) initialTargetBPS(options periodicOptions) float64 {
	if c == burstClassQualified {
		return options.PressureHighMinWriteBPS * 1.5
	}
	return options.BaselineTargetWriteBPS
}

type burstClassScheduler struct {
	configured burstClass
	random     *mathrand.Rand
	pending    burstClass
}

func newBurstClassScheduler(configured burstClass, random *mathrand.Rand) *burstClassScheduler {
	return &burstClassScheduler{configured: configured, random: random}
}

// next returns a seeded pair containing one transient and one qualified burst.
// This preserves random ordering without allowing an unbounded wait for a
// scheduler-qualifying burst in mixed mode.
func (s *burstClassScheduler) next() burstClass {
	if s.configured != burstClassMixed {
		return s.configured
	}
	if s.pending != "" {
		class := s.pending
		s.pending = ""
		return class
	}
	if s.random.Intn(2) == 0 {
		s.pending = burstClassQualified
		return burstClassTransient
	}
	s.pending = burstClassTransient
	return burstClassQualified
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

type floatEventValue float64

func (f floatEventValue) MarshalJSON() ([]byte, error) {
	value := strconv.FormatFloat(float64(f), 'f', -1, 64)
	if !strings.ContainsAny(value, ".eE") {
		value += ".0"
	}
	return []byte(value), nil
}

type benchmarkEvent struct {
	EventTSMS                 int64           `json:"event_ts_ms"`
	EventSequence             uint64          `json:"event_sequence"`
	RunID                     string          `json:"run_id"`
	EventType                 string          `json:"event_type"`
	Phase                     string          `json:"phase"`
	Cycle                     uint64          `json:"cycle"`
	TargetDatabase            string          `json:"target_database"`
	LogicalTable              string          `json:"logical_table"`
	PhysicalTable             string          `json:"physical_table"`
	TableID                   uint64          `json:"table_id"`
	RegionID                  uint64          `json:"region_id"`
	RegionNumber              uint64          `json:"region_number"`
	Partition                 string          `json:"partition"`
	PartitionDescription      string          `json:"partition_description"`
	PartitionExpression       string          `json:"partition_expression"`
	LeaderDatanode            string          `json:"leader_datanode"`
	RegionRole                string          `json:"region_role"`
	Engine                    string          `json:"engine"`
	WindowStartMS             int64           `json:"window_start_ms"`
	WindowEndMS               int64           `json:"window_end_ms"`
	ScheduledTSMS             int64           `json:"scheduled_ts_ms"`
	ActualTSMS                int64           `json:"actual_ts_ms"`
	ScheduleDelayMS           int64           `json:"schedule_delay_ms"`
	RequestCount              uint64          `json:"request_count"`
	SampleCount               uint64          `json:"sample_count"`
	PayloadBytes              uint64          `json:"payload_bytes"`
	SuccessCount              uint64          `json:"success_count"`
	ErrorCount                uint64          `json:"error_count"`
	WriteBPS                  floatEventValue `json:"write_bps"`
	LatencyP50MS              floatEventValue `json:"latency_p50_ms"`
	LatencyP95MS              floatEventValue `json:"latency_p95_ms"`
	LatencyMaxMS              floatEventValue `json:"latency_max_ms"`
	PressureThresholdBPS      floatEventValue `json:"pressure_threshold_bps"`
	ConsecutiveHighWindows    uint64          `json:"consecutive_high_windows"`
	RegionRows                uint64          `json:"region_rows"`
	WrittenBytesSinceOpen     uint64          `json:"written_bytes_since_open"`
	QueryCPUTimeMillis        uint64          `json:"query_cpu_time_millis"`
	QueryScannedBytes         uint64          `json:"query_scanned_bytes"`
	DiskSize                  uint64          `json:"disk_size"`
	MemtableSize              uint64          `json:"memtable_size"`
	ManifestSize              uint64          `json:"manifest_size"`
	SSTSize                   uint64          `json:"sst_size"`
	SSTNum                    uint64          `json:"sst_num"`
	IndexSize                 uint64          `json:"index_size"`
	IntervalMS                int64           `json:"interval_ms"`
	MemtableSizeDeltaBytes    int64           `json:"memtable_size_delta_bytes"`
	MemtableWriteBPSApprox    floatEventValue `json:"memtable_write_bps_approx"`
	WrittenBytesBPS           floatEventValue `json:"written_bytes_bps"`
	BurstClass                string          `json:"burst_class"`
	RegionNew                 bool            `json:"region_new"`
	CounterReset              bool            `json:"counter_reset"`
	MemtableDecreased         bool            `json:"memtable_decreased"`
	AnalysisContextIncomplete bool            `json:"analysis_context_incomplete"`
	AutopilotExpect           string          `json:"autopilot_expect"`
	AutopilotConfigHash       string          `json:"autopilot_config_hash"`
	AutopilotConfigSnapshot   string          `json:"autopilot_config_snapshot"`
	Error                     string          `json:"error"`
	Details                   string          `json:"details"`
}

type eventWriter struct {
	client        *http.Client
	endpoint      string
	authorization string
	mu            sync.Mutex
	sequence      uint64
	pending       []benchmarkEvent
	firstErr      error
}

func newEventWriter(options periodicOptions) *eventWriter {
	return &eventWriter{client: &http.Client{Timeout: 30 * time.Second}, endpoint: options.MonitoringURL, authorization: options.MonitoringAuthorization}
}

func (w *eventWriter) emit(ctx context.Context, event benchmarkEvent) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.appendLocked(event)
	if len(w.pending) >= 100 {
		if err := w.flushLocked(ctx); err != nil {
			log.Printf("periodic-burst: ingest benchmark events: %v", err)
		}
	}
}

// emitAndFlush makes lifecycle events queryable before the workload phase they describe begins.
func (w *eventWriter) emitAndFlush(ctx context.Context, event benchmarkEvent) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.appendLocked(event)
	return w.flushLocked(ctx)
}

func (w *eventWriter) emitWithSnapshot(ctx context.Context, event benchmarkEvent, bundle *regionSnapshotBundle) {
	if bundle == nil {
		w.emit(ctx, event)
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.appendLocked(withSnapshotDetails(event, bundle))
	for _, snapshotEvent := range bundle.events {
		w.appendLocked(withSnapshotDetails(snapshotEvent, bundle))
	}
	if len(w.pending) >= 100 {
		if err := w.flushLocked(ctx); err != nil {
			log.Printf("periodic-burst: ingest benchmark events: %v", err)
		}
	}
}

// emitWithSnapshotAndFlush binds a lifecycle event to the current region snapshot and persists it immediately.
func (w *eventWriter) emitWithSnapshotAndFlush(ctx context.Context, event benchmarkEvent, bundle *regionSnapshotBundle) error {
	if bundle == nil {
		return w.emitAndFlush(ctx, event)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.appendLocked(withSnapshotDetails(event, bundle))
	for _, snapshotEvent := range bundle.events {
		w.appendLocked(withSnapshotDetails(snapshotEvent, bundle))
	}
	return w.flushLocked(ctx)
}

func (w *eventWriter) appendLocked(event benchmarkEvent) {
	w.sequence++
	event.EventSequence = w.sequence
	if event.EventTSMS == 0 {
		event.EventTSMS = time.Now().UnixMilli()
	}
	w.pending = append(w.pending, event)
}

func (w *eventWriter) close(ctx context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.flushLocked(ctx)
}

func (w *eventWriter) flushLocked(ctx context.Context) error {
	if len(w.pending) == 0 {
		return w.firstErr
	}
	var body bytes.Buffer
	encoder := json.NewEncoder(&body)
	for _, event := range w.pending {
		if err := encoder.Encode(event); err != nil {
			w.recordError(err)
			w.pending = nil
			return err
		}
	}
	endpoint, err := url.Parse(w.endpoint)
	if err != nil {
		w.recordError(err)
		w.pending = nil
		return err
	}
	query := endpoint.Query()
	query.Set("db", "public")
	query.Set("table", "benchmark_autopilot_events")
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
					w.firstErr = nil
					return nil
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
	return lastErr
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

// bytePacer reserves compressed remote-write payloads at a global target rate.
// A single pacer is shared by all workers so --workers cannot multiply pressure.
type bytePacer struct {
	mu          sync.Mutex
	targetBPS   float64
	nextAllowed time.Time
}

// burstAmplifier raises config-derived passes only when the qualified traffic
// cannot meet its target rate. The cap keeps a bad target from growing an
// unbounded producer backlog.
type burstAmplifier struct {
	mu      sync.Mutex
	current uint64
	max     uint64
}

func newBurstAmplifier(initial uint64) *burstAmplifier {
	return &burstAmplifier{current: initial, max: initial * 4}
}

func (a *burstAmplifier) value() uint64 {
	if a == nil {
		return 0
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.current
}

func (a *burstAmplifier) increase() (uint64, bool) {
	if a == nil {
		return 0, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.current >= a.max {
		return a.current, false
	}
	next := uint64(math.Ceil(float64(a.current) * 1.25))
	if next <= a.current {
		next = a.current + 1
	}
	a.current = min(next, a.max)
	return a.current, true
}

func newBytePacer(targetBPS float64) *bytePacer {
	if targetBPS <= 0 {
		return nil
	}
	return &bytePacer{targetBPS: targetBPS}
}

func (p *bytePacer) target() float64 {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.targetBPS
}

func (p *bytePacer) increase(maxBPS float64) (float64, bool) {
	if p == nil || maxBPS <= 0 {
		return 0, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	previous := p.targetBPS
	p.targetBPS = math.Min(maxBPS, p.targetBPS*1.25)
	return p.targetBPS, p.targetBPS > previous
}

func (p *bytePacer) wait(ctx context.Context, payloadBytes int) error {
	if p == nil || payloadBytes == 0 {
		return nil
	}
	p.mu.Lock()
	now := time.Now()
	start := p.nextAllowed
	if start.Before(now) {
		start = now
	}
	// Snapshot the rate while reserving, so later adaptive changes only affect
	// payloads that have not yet entered the pacing queue.
	p.nextAllowed = start.Add(time.Duration(float64(payloadBytes) / p.targetBPS * float64(time.Second)))
	p.mu.Unlock()

	if delay := time.Until(start); delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
	return nil
}

func periodicWorker(ctx context.Context, url, authorization, physicalTable string, requests <-chan prompb.WriteRequest, results chan<- writeResult, wg *sync.WaitGroup, dryRun bool, pacer *bytePacer) {
	defer wg.Done()
	requester := benchhttp.NewRequester(url)
	if authorization != "" {
		requester.SetHeader("Authorization", authorization)
	}
	requester.SetHeader("x-greptime-hint-physical_table", physicalTable)
	for request := range requests {
		result := writeResult{requestCount: 1, sampleCount: sampleCount(request)}
		started := time.Now()
		if dryRun {
			result.payloadBytes = 0
		} else {
			prepared, err := benchhttp.PrepareWriteRequest(request)
			if err == nil {
				result.payloadBytes = uint64(prepared.PayloadBytes())
				err = pacer.wait(ctx, prepared.PayloadBytes())
			}
			if err == nil {
				err = requester.SendPrepared(ctx, prepared)
			}
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
	configTables, logicalTables := configTables(fileConfigs)
	logicalTable := ""
	if len(logicalTables) == 1 {
		logicalTable = logicalTables[0]
	}
	baseEvent := benchmarkEvent{RunID: runID, TargetDatabase: options.TargetDatabase, LogicalTable: logicalTable, PhysicalTable: options.TargetPhysicalTable, AutopilotExpect: options.AutopilotExpect, AutopilotConfigHash: options.AutopilotConfigHash, AutopilotConfigSnapshot: options.AutopilotConfigSnapshot, AnalysisContextIncomplete: options.AutopilotConfigSnapshot == ""}
	var writer *eventWriter
	var snapshots *regionSnapshotStore
	var refresher *regionRefresher
	emitLifecycle := func(name string, event benchmarkEvent, bundle *regionSnapshotBundle) {
		if writer == nil {
			return
		}
		if err := writer.emitWithSnapshotAndFlush(ctx, event, bundle); err != nil {
			// Self-monitoring is diagnostic-only. Never make a telemetry ingestion
			// outage stop the workload that it is meant to describe.
			log.Printf("periodic-burst: write %s event: %v; continuing workload", name, err)
		}
	}

	if !s.DryRun {
		writer = newEventWriter(options)
		snapshots = &regionSnapshotStore{}
		client := httpSQLClient{endpoint: options.ObserveSQLURL, database: options.TargetDatabase, authorization: s.authorizationHeader(), client: &http.Client{Timeout: 30 * time.Second}}
		refresher = &regionRefresher{client: client, physicalTable: options.TargetPhysicalTable, configTables: configTables, logicalTables: logicalTables, base: baseEvent, previous: make(map[uint64]regionSnapshot), store: snapshots}
		bundle, changes, err := refresher.refresh(ctx)
		if err != nil {
			_ = writer.close(context.Background())
			return fmt.Errorf("initial region snapshot: %w", err)
		}
		for _, event := range changes {
			writer.emitWithSnapshot(ctx, event, bundle)
		}
	}

	observerContext, cancelObserver := context.WithCancel(ctx)
	var observerDone chan struct{}
	if refresher != nil {
		observerDone = make(chan struct{})
		go func() {
			defer close(observerDone)
			refresher.run(observerContext, options.ObserveInterval, writer)
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
			emitWithCurrentSnapshot(writer, finishContext, eventWith(baseEvent, "run_failed", "failed", 0, func(event *benchmarkEvent) {
				event.Error = runErr.Error()
			}), snapshots)
		}
		if closeErr := writer.close(finishContext); closeErr != nil {
			log.Printf("periodic-burst: flush benchmark events: %v", closeErr)
		}
	}()

	current := time.Now()
	churnEpochGenerator := samples.NewChurnEpochGenerator(s.ChurnInterval)
	random, seed := newPeriodicRandom(options.RandomSeed)
	classScheduler := newBurstClassScheduler(options.BurstClass, random)
	firstBurstAt := time.Now().Add(jitterDuration(options.BaselineDuration, options.BurstJitter, random))
	emitLifecycle("run_started", eventWith(baseEvent, "run_started", "baseline", 0, func(event *benchmarkEvent) {
		event.Details = fmt.Sprintf(`{"baseline_duration":"%s","baseline_tick_interval":"%s","burst_active_duration":"%s","transient_burst_duration":"%s","burst_period":"%s","burst_gap":"%s","burst_jitter":%g,"burst_amplification":%d,"burst_count":%d,"random_seed":%d,"observe_interval":"%s","periodic_traffic_mode":%q,"burst_class":%q,"baseline_target_write_bps":%g,"qualified_max_write_bps":%g,"pressure_high_min_write_bps":%g,"configured_logical_tables":%s}`, options.BaselineDuration, options.BaselineTickInterval, options.BurstActiveDuration, options.TransientBurstDuration, options.BurstPeriod, options.BurstGap, options.BurstJitter, options.BurstAmplification, options.BurstCount, seed, options.ObserveInterval, options.TrafficMode, options.BurstClass, options.BaselineTargetWriteBPS, options.QualifiedMaxWriteBPS, options.PressureHighMinWriteBPS, mustJSON(logicalTables))
	}), currentSnapshot(snapshots))
	for cycle := uint64(1); options.BurstCount == 0 || cycle <= options.BurstCount; cycle++ {
		class := classScheduler.next()
		plannedTarget, targetOK := selectHotTarget(currentSnapshot(snapshots), random)
		emitLifecycle("pressure_scheduled", plannedBurstEvent(baseEvent, cycle, firstBurstAt, options, seed, class, plannedTarget, targetOK), currentSnapshot(snapshots))
		if err := s.runPeriodicWrites(ctx, fileConfigs, &current, churnEpochGenerator, firstBurstAt, options.BaselineTickInterval, "baseline", 0, options, writer, snapshots, baseEvent, nil, burstClassTransient); err != nil {
			return err
		}
		targetReselected := false
		previousTarget := plannedTarget
		if !hasHotTarget(currentSnapshot(snapshots), plannedTarget) {
			plannedTarget, targetOK = selectHotTarget(currentSnapshot(snapshots), random)
			targetReselected = true
		}
		if writer != nil && targetReselected {
			emitLifecycle("pressure_target_reselected", eventWith(baseEvent, "pressure_target_reselected", "baseline", cycle, func(event *benchmarkEvent) {
				event.RegionID = plannedTarget.regionID
				event.Details = fmt.Sprintf(`{"reason":"partition_mapping_changed","previous_region_id":%d,"previous_label_name":%q,"previous_label_value":%q,"hot_label_name":%q,"hot_label_value":%q}`, previousTarget.regionID, previousTarget.labelName, previousTarget.labelValue, plannedTarget.labelName, plannedTarget.labelValue)
			}), currentSnapshot(snapshots))
		}
		if writer != nil {
			emitLifecycle("pressure_started", eventWith(baseEvent, "pressure_started", "active", cycle, func(event *benchmarkEvent) {
				now := time.Now()
				event.ScheduledTSMS = firstBurstAt.UnixMilli()
				event.ActualTSMS = now.UnixMilli()
				event.BurstClass = string(class)
				event.AutopilotExpect = class.expectation(options.AutopilotExpect)
				event.PressureThresholdBPS = floatEventValue(class.initialTargetBPS(options))
				if targetOK {
					event.RegionID = plannedTarget.regionID
					event.Details = fmt.Sprintf(`{"hot_label_name":%q,"hot_label_value":%q,"burst_class":%q,"target_write_bps":%g}`, plannedTarget.labelName, plannedTarget.labelValue, class, class.initialTargetBPS(options))
				}
			}), currentSnapshot(snapshots))
		}
		activeDeadline := time.Now().Add(class.duration(options))
		var target *hotTarget
		if targetOK {
			target = &plannedTarget
		}
		if err := s.runPeriodicWrites(ctx, fileConfigs, &current, churnEpochGenerator, activeDeadline, s.TickInterval, "active", cycle, options, writer, snapshots, baseEvent, target, class); err != nil {
			return err
		}
		if writer != nil {
			emitWithCurrentSnapshot(writer, ctx, eventWith(baseEvent, "phase_completed", "active", cycle, func(event *benchmarkEvent) {
				event.Details = "active requests drained"
			}), snapshots)
		}
		firstBurstAt = nextBurstStart(options, firstBurstAt, time.Now(), random)
	}

	cancelObserver()
	if observerDone != nil {
		<-observerDone
	}
	observerStopped = true
	if writer == nil {
		return nil
	}
	emitWithCurrentSnapshot(writer, ctx, eventWith(baseEvent, "run_completed", "completed", 0, nil), snapshots)
	return nil
}

func nextBurstStart(options periodicOptions, previousStart, activeFinished time.Time, random *mathrand.Rand) time.Time {
	if options.TrafficMode == "steady" {
		return activeFinished.Add(options.BurstGap)
	}
	next := previousStart.Add(jitterDuration(options.BurstPeriod, options.BurstJitter, random))
	if minimum := activeFinished.Add(options.BaselineTickInterval); next.Before(minimum) {
		return minimum
	}
	return next
}

func (s *SampleLoader) runPeriodicWrites(ctx context.Context, fileConfigs []samples.FileConfig, current *time.Time, churn *samples.ChurnEpochGenerator, deadline time.Time, tickInterval time.Duration, phase string, cycle uint64, options periodicOptions, writer *eventWriter, snapshots *regionSnapshotStore, base benchmarkEvent, target *hotTarget, class burstClass) error {
	phaseCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
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
	var pacer *bytePacer
	var amplifier *burstAmplifier
	if options.TrafficMode == "steady" {
		targetBPS := options.BaselineTargetWriteBPS
		if phase == "active" {
			targetBPS = class.initialTargetBPS(options)
		}
		pacer = newBytePacer(targetBPS)
		if phase == "active" && class == burstClassQualified {
			amplifier = newBurstAmplifier(options.BurstAmplification)
		}
	}
	for i := 0; i < s.Workers; i++ {
		workers.Add(1)
		go periodicWorker(ctx, s.RemoteWriteURL, s.authorizationHeader(), options.TargetPhysicalTable, requests, results, &workers, s.DryRun, pacer)
	}

	started := time.Now()
	windowInterval := workloadWindowInterval
	if options.TrafficMode == "steady" {
		// In steady mode the configured scrape cadence is also the observation
		// window. This makes the recorded rate directly comparable to Prometheus.
		windowInterval = tickInterval
	}
	window := time.NewTicker(workloadWindowInterval)
	if windowInterval != workloadWindowInterval {
		window.Stop()
		window = time.NewTicker(windowInterval)
	}
	defer window.Stop()
	windowStarted := started
	consecutiveHigh := uint64(0)
	producerDone := make(chan struct{})
	go func() {
		defer close(producerDone)
		defer close(requests)
		tick := time.NewTicker(tickInterval)
		defer tick.Stop()
		generate := func() {
			epoch := churn.GetChurnEpoch()
			repetitions := uint64(1)
			if target != nil {
				repetitions = options.BurstAmplification
				if amplifier != nil {
					repetitions = amplifier.value()
				}
			}
			for range repetitions {
				if target == nil {
					s.convertToRemoteWriteRequestsStreaming(phaseCtx, fileConfigs, *current, requests, epoch)
				} else {
					s.convertToRemoteWriteRequestsStreamingFiltered(phaseCtx, fileConfigs, *current, requests, epoch, func(series prompb.TimeSeries) bool {
						return matchesHotTarget(series, *target)
					})
				}
				if phaseCtx.Err() != nil {
					return
				}
				*current = current.Add(s.Interval)
			}
		}
		for {
			generate()
			select {
			case <-phaseCtx.Done():
				return
			case <-tick.C:
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			cancel()
			<-producerDone
			workers.Wait()
			close(results)
			<-collectorDone
			return ctx.Err()
		case <-phaseCtx.Done():
			<-producerDone
			workers.Wait()
			close(results)
			<-collectorDone
			if err := ctx.Err(); err != nil {
				return err
			}
			now := time.Now()
			emitWorkloadWindow(ctx, writer, snapshots, base, phase, cycle, windowStarted, now, collector.reset(), options.PressureHighMinWriteBPS, consecutiveHigh, pacer, amplifier, class, options.QualifiedMaxWriteBPS)
			return nil
		case now := <-window.C:
			consecutiveHigh = emitWorkloadWindow(ctx, writer, snapshots, base, phase, cycle, windowStarted, now, collector.reset(), options.PressureHighMinWriteBPS, consecutiveHigh, pacer, amplifier, class, options.QualifiedMaxWriteBPS)
			windowStarted = now
		}
	}
}

func emitWorkloadWindow(ctx context.Context, writer *eventWriter, snapshots *regionSnapshotStore, base benchmarkEvent, phase string, cycle uint64, start, end time.Time, metrics workloadMetrics, threshold float64, consecutiveHigh uint64, pacer *bytePacer, amplifier *burstAmplifier, class burstClass, qualifiedMaxWriteBPS float64) uint64 {
	if writer == nil || !end.After(start) {
		return consecutiveHigh
	}
	latencies := append([]time.Duration(nil), metrics.latencies...)
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	duration := end.Sub(start)
	writeBPS := float64(metrics.payloadBytes) / duration.Seconds()
	event := eventWith(base, "workload_window", phase, cycle, func(event *benchmarkEvent) {
		event.WindowStartMS = start.UnixMilli()
		event.WindowEndMS = end.UnixMilli()
		event.RequestCount = metrics.requestCount
		event.SampleCount = metrics.sampleCount
		event.PayloadBytes = metrics.payloadBytes
		event.SuccessCount = metrics.successCount
		event.ErrorCount = metrics.errorCount
		event.WriteBPS = floatEventValue(writeBPS)
		event.LatencyP50MS = floatEventValue(latencyPercentileMS(latencies, 0.50))
		event.LatencyP95MS = floatEventValue(latencyPercentileMS(latencies, 0.95))
		event.LatencyMaxMS = floatEventValue(latencyPercentileMS(latencies, 1))
		event.PressureThresholdBPS = floatEventValue(threshold)
		event.BurstClass = string(class)
		if target := pacer.target(); target > 0 {
			event.Details = fmt.Sprintf(`{"target_write_bps":%g,"source_passes":%d}`, target, amplifier.value())
		}
	})
	if writeBPS >= threshold && metrics.errorCount == 0 {
		consecutiveHigh++
	} else {
		if consecutiveHigh >= 3 {
			emitWithCurrentSnapshot(writer, ctx, eventWith(base, "pressure_dropped", phase, cycle, func(event *benchmarkEvent) {
				event.WriteBPS = floatEventValue(writeBPS)
				event.PressureThresholdBPS = floatEventValue(threshold)
				event.ConsecutiveHighWindows = consecutiveHigh
			}), snapshots)
		}
		consecutiveHigh = 0
	}
	event.ConsecutiveHighWindows = consecutiveHigh
	emitWithCurrentSnapshot(writer, ctx, event, snapshots)
	if metrics.errorCount > 0 {
		emitWithCurrentSnapshot(writer, ctx, eventWith(base, "write_error", phase, cycle, func(event *benchmarkEvent) {
			event.WindowStartMS = start.UnixMilli()
			event.WindowEndMS = end.UnixMilli()
			event.ErrorCount = metrics.errorCount
			event.Error = metrics.lastError
		}), snapshots)
	}
	if consecutiveHigh == 3 {
		emitWithCurrentSnapshot(writer, ctx, eventWith(base, "pressure_observed_high", phase, cycle, func(event *benchmarkEvent) {
			event.WriteBPS = floatEventValue(writeBPS)
			event.PressureThresholdBPS = floatEventValue(threshold)
			event.ConsecutiveHighWindows = consecutiveHigh
		}), snapshots)
	}
	if phase == "active" && class == burstClassQualified && metrics.errorCount == 0 && pacer != nil && writeBPS < pacer.target()*0.9 {
		if target, increased := pacer.increase(qualifiedMaxWriteBPS); increased {
			sourcePasses, _ := amplifier.increase()
			emitWithCurrentSnapshot(writer, ctx, eventWith(base, "pressure_rate_adjusted", phase, cycle, func(event *benchmarkEvent) {
				event.BurstClass = string(class)
				event.WriteBPS = floatEventValue(writeBPS)
				event.PressureThresholdBPS = floatEventValue(target)
				event.Details = fmt.Sprintf(`{"reason":"observed_below_target","new_target_write_bps":%g,"max_target_write_bps":%g,"source_passes":%d}`, target, qualifiedMaxWriteBPS, sourcePasses)
			}), snapshots)
		}
	}
	return consecutiveHigh
}

func emitWithCurrentSnapshot(writer *eventWriter, ctx context.Context, event benchmarkEvent, snapshots *regionSnapshotStore) {
	if snapshots == nil {
		writer.emit(ctx, event)
		return
	}
	writer.emitWithSnapshot(ctx, event, snapshots.get())
}

func mustJSON(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "[]"
	}
	return string(encoded)
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

func newPeriodicRandom(configuredSeed int64) (*mathrand.Rand, int64) {
	if configuredSeed != 0 {
		return mathrand.New(mathrand.NewSource(configuredSeed)), configuredSeed
	}
	var bytes [8]byte
	if _, err := cryptorand.Read(bytes[:]); err == nil {
		configuredSeed = int64(binary.LittleEndian.Uint64(bytes[:]))
	} else {
		configuredSeed = time.Now().UnixNano()
	}
	return mathrand.New(mathrand.NewSource(configuredSeed)), configuredSeed
}

func jitterDuration(duration time.Duration, jitter float64, random *mathrand.Rand) time.Duration {
	if duration == 0 || jitter == 0 {
		return duration
	}
	factor := 1 - jitter + 2*jitter*random.Float64()
	return time.Duration(float64(duration) * factor)
}

func plannedBurstEvent(base benchmarkEvent, cycle uint64, scheduled time.Time, options periodicOptions, seed int64, class burstClass, target hotTarget, targetOK bool) benchmarkEvent {
	return eventWith(base, "pressure_scheduled", "baseline", cycle, func(event *benchmarkEvent) {
		event.ScheduledTSMS = scheduled.UnixMilli()
		event.BurstClass = string(class)
		event.AutopilotExpect = class.expectation(options.AutopilotExpect)
		event.PressureThresholdBPS = floatEventValue(class.initialTargetBPS(options))
		if targetOK {
			event.RegionID = target.regionID
			event.Details = fmt.Sprintf(`{"random_seed":%d,"burst_class":%q,"burst_duration":"%s","burst_period":"%s","burst_gap":"%s","burst_jitter":%g,"burst_amplification":%d,"target_write_bps":%g,"hot_label_name":%q,"hot_label_value":%q}`, seed, class, class.duration(options), options.BurstPeriod, options.BurstGap, options.BurstJitter, options.BurstAmplification, class.initialTargetBPS(options), target.labelName, target.labelValue)
		} else {
			event.Details = fmt.Sprintf(`{"random_seed":%d,"burst_class":%q,"burst_duration":"%s","burst_period":"%s","burst_gap":"%s","burst_jitter":%g,"burst_amplification":%d,"target_write_bps":%g,"hot_target_unavailable":true}`, seed, class, class.duration(options), options.BurstPeriod, options.BurstGap, options.BurstJitter, options.BurstAmplification, class.initialTargetBPS(options))
		}
	})
}

func newRunID() string {
	bytes := make([]byte, 8)
	if _, err := cryptorand.Read(bytes); err != nil {
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
	partitionDescription  string
	partitionExpression   string
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

type regionSnapshotBundle struct {
	id             string
	timestamp      time.Time
	mappingVersion uint64
	snapshots      []regionSnapshot
	events         []benchmarkEvent
	distribution   []sharedpartition.RegionDistribution
	logicalTables  []string
	hotTargets     []hotTarget
	refreshError   string
	stale          bool
}

// hotTarget is a config-derived label value that routes writes to one current region.
type hotTarget struct {
	regionID   uint64
	labelName  string
	labelValue string
}

type regionSnapshotStore struct {
	mu      sync.RWMutex
	current *regionSnapshotBundle
}

func (s *regionSnapshotStore) set(bundle *regionSnapshotBundle) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.current = bundle
}

func (s *regionSnapshotStore) get() *regionSnapshotBundle {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.current
}

func currentSnapshot(store *regionSnapshotStore) *regionSnapshotBundle {
	if store == nil {
		return nil
	}
	return store.get()
}

func withSnapshotDetails(event benchmarkEvent, bundle *regionSnapshotBundle) benchmarkEvent {
	details := map[string]any{}
	if event.Details != "" {
		if err := json.Unmarshal([]byte(event.Details), &details); err != nil {
			details["message"] = event.Details
		}
	}
	details["snapshot_id"] = bundle.id
	details["snapshot_ts_ms"] = bundle.timestamp.UnixMilli()
	details["mapping_version"] = bundle.mappingVersion
	details["snapshot_stale"] = bundle.stale
	if bundle.refreshError != "" {
		details["snapshot_refresh_error"] = bundle.refreshError
	}
	encoded, err := json.Marshal(details)
	if err == nil {
		event.Details = string(encoded)
	}
	return event
}

func configTables(fileConfigs []samples.FileConfig) ([]sharedpartition.ConfigTable, []string) {
	tables := make([]sharedpartition.ConfigTable, 0, len(fileConfigs))
	names := make([]string, 0, len(fileConfigs))
	for _, fileConfig := range fileConfigs {
		values := make(map[string][]string, len(fileConfig.Config.Tags))
		for _, tag := range fileConfig.Config.Tags {
			values[tag.Name] = tag.Dist.LabelGenerator().All()
		}
		tables = append(tables, sharedpartition.ConfigTable{Name: fileConfig.Name, LabelValues: values, SeriesCount: uint64(fileConfig.SeriesCount)})
		names = append(names, fileConfig.Name)
	}
	sort.Strings(names)
	return tables, names
}

func configHotTargets(definition sharedpartition.PartitionDefinition, metadata []sharedpartition.Metadata, tables []sharedpartition.ConfigTable) []hotTarget {
	if len(definition.Columns) != 1 {
		return nil
	}
	labelName := definition.Columns[0]
	seen := make(map[string]struct{})
	targets := make([]hotTarget, 0)
	for _, table := range tables {
		for _, value := range table.LabelValues[labelName] {
			item, _, ok := sharedpartition.FindMetadataByColumnValue(definition, metadata, labelName, value)
			if !ok || item.RegionID == 0 {
				continue
			}
			key := fmt.Sprintf("%d/%s/%s", item.RegionID, labelName, value)
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			targets = append(targets, hotTarget{regionID: item.RegionID, labelName: labelName, labelValue: value})
		}
	}
	return targets
}

func selectHotTarget(bundle *regionSnapshotBundle, random *mathrand.Rand) (hotTarget, bool) {
	if bundle == nil || len(bundle.hotTargets) == 0 {
		return hotTarget{}, false
	}
	return bundle.hotTargets[random.Intn(len(bundle.hotTargets))], true
}

func hasHotTarget(bundle *regionSnapshotBundle, target hotTarget) bool {
	if bundle == nil {
		return false
	}
	for _, current := range bundle.hotTargets {
		if current == target {
			return true
		}
	}
	return false
}

func matchesHotTarget(series prompb.TimeSeries, target hotTarget) bool {
	for _, label := range series.Labels {
		if label.Name == target.labelName && label.Value == target.labelValue {
			return true
		}
	}
	return false
}

type regionRefresher struct {
	client        httpSQLClient
	physicalTable string
	configTables  []sharedpartition.ConfigTable
	logicalTables []string
	base          benchmarkEvent
	previous      map[uint64]regionSnapshot
	version       uint64
	store         *regionSnapshotStore
}

func (r *regionRefresher) refresh(ctx context.Context) (*regionSnapshotBundle, []benchmarkEvent, error) {
	now := time.Now()
	snapshots, err := r.client.regionSnapshots(ctx, r.physicalTable, now)
	if err != nil {
		return nil, nil, err
	}
	metadata, err := r.client.partitionMetadata(ctx, r.physicalTable)
	if err != nil {
		return nil, nil, err
	}
	showCreate, err := r.client.showCreateTable(ctx, r.physicalTable)
	if err != nil {
		return nil, nil, err
	}
	definition, err := sharedpartition.ParsePartitionDefinition(showCreate)
	if err != nil {
		return nil, nil, fmt.Errorf("parse physical table partition definition: %w", err)
	}
	distribution, err := sharedpartition.ConfigRegionDistribution(&definition, metadata, r.configTables)
	if err != nil {
		return nil, nil, err
	}
	r.version++
	bundle := &regionSnapshotBundle{id: newSnapshotID(), timestamp: now, mappingVersion: r.version, snapshots: snapshots, distribution: distribution, logicalTables: r.logicalTables, hotTargets: configHotTargets(definition, metadata, r.configTables)}
	for _, snapshot := range snapshots {
		prior, exists := r.previous[snapshot.regionID]
		bundle.events = append(bundle.events, regionEvent(r.base, snapshot, exists, prior))
	}
	changes := mappingChanges(r.base, bundle, r.previous)
	r.previous = make(map[uint64]regionSnapshot, len(snapshots))
	for _, snapshot := range snapshots {
		r.previous[snapshot.regionID] = snapshot
	}
	r.store.set(bundle)
	return bundle, changes, nil
}

func mappingChanges(base benchmarkEvent, bundle *regionSnapshotBundle, previous map[uint64]regionSnapshot) []benchmarkEvent {
	changes := []benchmarkEvent{eventWith(base, "partition_mapping_snapshot", "observe", 0, func(event *benchmarkEvent) {
		details, _ := json.Marshal(map[string]any{"configured_logical_tables": bundle.logicalTables, "region_distribution": bundle.distribution})
		event.Details = string(details)
	})}
	seen := make(map[uint64]struct{}, len(bundle.snapshots))
	for _, snapshot := range bundle.snapshots {
		seen[snapshot.regionID] = struct{}{}
		prior, exists := previous[snapshot.regionID]
		current := regionEvent(base, snapshot, exists, prior)
		if !exists {
			changes = append(changes, eventWith(current, "region_created", "observe", 0, nil))
		} else if prior.leader != snapshot.leader {
			changes = append(changes, eventWith(current, "leader_changed", "observe", 0, nil))
		} else if prior.partition != snapshot.partition || prior.partitionDescription != snapshot.partitionDescription || prior.partitionExpression != snapshot.partitionExpression {
			changes = append(changes, eventWith(current, "partition_mapping_changed", "observe", 0, nil))
		}
	}
	for regionID, snapshot := range previous {
		if _, ok := seen[regionID]; !ok {
			changes = append(changes, eventWith(regionEvent(base, snapshot, true, snapshot), "region_disappeared", "observe", 0, nil))
		}
	}
	return changes
}

func newSnapshotID() string {
	return fmt.Sprintf("snapshot-%d", time.Now().UnixNano())
}

func (r *regionRefresher) run(ctx context.Context, interval time.Duration, writer *eventWriter) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			bundle, changes, err := r.refresh(ctx)
			if err != nil {
				current := r.store.get()
				if current != nil {
					stale := *current
					stale.stale = true
					stale.refreshError = err.Error()
					r.store.set(&stale)
					writer.emitWithSnapshot(ctx, eventWith(r.base, "snapshot_refresh_error", "observe", 0, func(event *benchmarkEvent) { event.Error = err.Error() }), &stale)
				} else {
					writer.emit(ctx, eventWith(r.base, "snapshot_refresh_error", "observe", 0, func(event *benchmarkEvent) { event.Error = err.Error() }))
				}
				continue
			}
			for _, event := range changes {
				writer.emitWithSnapshot(ctx, event, bundle)
			}
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
		event.PartitionDescription = snapshot.partitionDescription
		event.PartitionExpression = snapshot.partitionExpression
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
		event.WrittenBytesBPS = floatEventValue(float64(snapshot.writtenBytesSinceOpen-previous.writtenBytesSinceOpen) / elapsed.Seconds())
	}
	if event.MemtableSizeDeltaBytes >= 0 {
		event.MemtableWriteBPSApprox = floatEventValue(float64(event.MemtableSizeDeltaBytes) / elapsed.Seconds())
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
	sql := `SELECT p.partition_name, p.partition_expression, p.partition_description, p.greptime_partition_id, rp.peer_id, s.table_id, s.region_number, s.region_rows, s.written_bytes_since_open, s.query_cpu_time_millis, s.query_scanned_bytes, s.disk_size, s.memtable_size, s.manifest_size, s.sst_size, s.sst_num, s.index_size, s.engine, s.region_role FROM information_schema.partitions AS p LEFT JOIN information_schema.region_statistics AS s ON p.greptime_partition_id = s.region_id LEFT JOIN information_schema.region_peers AS rp ON s.region_id = rp.region_id AND rp.is_leader = 'Yes' WHERE p.table_schema = ` + quoteSQLLiteral(c.database) + ` AND p.table_name = ` + quoteSQLLiteral(physicalTable) + ` ORDER BY p.partition_ordinal_position`
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
		result = append(result, regionSnapshot{timestamp: now, partition: stringValue(row["partition_name"]), partitionExpression: stringValue(row["partition_expression"]), partitionDescription: stringValue(row["partition_description"]), regionID: regionID, leader: stringValue(row["peer_id"]), tableID: uintValue(row["table_id"]), regionNumber: uintValue(row["region_number"]), regionRows: uintValue(row["region_rows"]), writtenBytesSinceOpen: uintValue(row["written_bytes_since_open"]), queryCPUTimeMillis: uintValue(row["query_cpu_time_millis"]), queryScannedBytes: uintValue(row["query_scanned_bytes"]), diskSize: uintValue(row["disk_size"]), memtableSize: uintValue(row["memtable_size"]), manifestSize: uintValue(row["manifest_size"]), sstSize: uintValue(row["sst_size"]), sstNum: uintValue(row["sst_num"]), indexSize: uintValue(row["index_size"]), engine: stringValue(row["engine"]), role: stringValue(row["region_role"])})
	}
	return result, nil
}

func (c httpSQLClient) partitionMetadata(ctx context.Context, physicalTable string) ([]sharedpartition.Metadata, error) {
	sql := `SELECT partition_name, partition_ordinal_position, partition_expression, partition_description, greptime_partition_id FROM information_schema.partitions WHERE table_schema = ` + quoteSQLLiteral(c.database) + ` AND table_name = ` + quoteSQLLiteral(physicalTable) + ` ORDER BY partition_ordinal_position`
	rows, err := c.query(ctx, sql)
	if err != nil {
		return nil, err
	}
	metadata := make([]sharedpartition.Metadata, 0, len(rows))
	for _, row := range rows {
		metadata = append(metadata, sharedpartition.Metadata{Name: stringValue(row["partition_name"]), Ordinal: uintValue(row["partition_ordinal_position"]), Expression: stringValue(row["partition_expression"]), Description: stringValue(row["partition_description"]), RegionID: uintValue(row["greptime_partition_id"])})
	}
	return metadata, nil
}

func (c httpSQLClient) showCreateTable(ctx context.Context, physicalTable string) (string, error) {
	rows, err := c.query(ctx, "SHOW CREATE TABLE "+quoteSQLIdentifier(physicalTable))
	if err != nil {
		return "", err
	}
	if len(rows) != 1 {
		return "", fmt.Errorf("SHOW CREATE TABLE returned %d rows", len(rows))
	}
	for _, value := range rows[0] {
		create := stringValue(value)
		if strings.Contains(strings.ToUpper(create), "CREATE TABLE") {
			return create, nil
		}
	}
	return "", fmt.Errorf("SHOW CREATE TABLE returned no definition")
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

func quoteSQLIdentifier(value string) string {
	return "`" + strings.ReplaceAll(value, "`", "``") + "`"
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
