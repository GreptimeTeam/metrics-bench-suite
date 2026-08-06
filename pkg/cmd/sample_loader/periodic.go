package sampleloader

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
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
	defaultAutopilotWindow  = 45 * time.Second
	defaultAutopilotSamples = 3
	defaultMaxHistoryCV     = 0.2
	minPressureScale        = 0.8
	maxPressureScale        = 1.2
	minTransientScale       = 0.35
	maxTransientScale       = 0.7
	minCaseBurstPasses      = 128
	// Keep two full scheduler windows after the retained history is expected to
	// contain only the new plateau. One extra observation replaces the oldest
	// sample; two more guard against the tick boundary and a memtable flush.
	autopilotSettlingWindows = 2
	// A scheduler baseline needs enough samples per request to keep a region
	// above the default 10 KiB/s movable-load floor without repeatedly sending
	// many tiny remote-write requests.
	stableBackgroundMinSeries = 19200
	// AutoRepartition is driven by a region-to-table load ratio and history CV,
	// not a datanode absolute-load floor. A higher generic burst target merely
	// makes the target region flush cyclically and invalidates that history.
	// The frontend's compressed remote-write payload expands substantially in a
	// metric-region memtable. Keep it below the 512 MiB region flush trigger for
	// the full scheduler history horizon; auto repartition only requires a
	// relative load imbalance, not a RegionBalancer-like absolute BPS floor.
	stableRepartitionTargetBPS = float64(128 * 1024)
	// Small scheduler batches make this intentionally low rate continuous at
	// the three-second MetaSrv heartbeat cadence, instead of a larger request
	// followed by a multi-second pacer pause.
	stableRepartitionBatchSamples = 128
	// A stable-region case has one byte pacer per selected target value plus a
	// dedicated all-region baseline lane. A single sender per target leaves the
	// pacer starved by request/HTTP overhead and can stay below the Enterprise
	// movable-region threshold even though the planned BPS is high enough.
	stableTargetWorkers   = 5
	caseBaselineRatio     = 0.25
	defaultHistoryWindows = 5
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
	AutopilotCase           string
	AutopilotRateMode       string
	AutopilotBatchSamples   int
	PressureHighMinWriteBPS float64
	TrafficMode             string
	BurstClass              burstClass
	BaselineTargetWriteBPS  float64
	QualifiedMaxWriteBPS    float64
	MonitoringURL           string
	MonitoringAuthorization string
	AutopilotConfigSnapshot string
	AutopilotConfigHash     string
	AutopilotSchedule       autopilotSchedule
}

// autopilotSchedule contains only the source-level timing guards needed to
// construct a safe transient probe. Defaults mirror the Enterprise options and
// are overridden by an effective TOML snapshot when present.
type autopilotSchedule struct {
	samplingWindow      time.Duration
	maxHistoryWindows   int
	repartitionSamples  int
	maxRegionHistoryCV  float64
	writeStableWindows  int
	migrationMinSamples int
	// Rebalance uses a source datanode high-load threshold, unlike
	// repartition's per-region history check. Keep these source-derived values
	// with the other effective scheduler settings so a migration plan can be
	// made schedulable even when the user chose a smaller generic pressure flag.
	rebalanceMinLoadBPS          float64
	rebalanceAcceptableLoadRatio float64
}

func defaultAutopilotSchedule() autopilotSchedule {
	return autopilotSchedule{
		samplingWindow:               defaultAutopilotWindow,
		maxHistoryWindows:            defaultHistoryWindows,
		repartitionSamples:           defaultAutopilotSamples,
		maxRegionHistoryCV:           defaultMaxHistoryCV,
		writeStableWindows:           2,
		migrationMinSamples:          defaultAutopilotSamples,
		rebalanceMinLoadBPS:          float64(4 * 1024 * 1024),
		rebalanceAcceptableLoadRatio: 0.12,
	}
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
	options.AutopilotSchedule = defaultAutopilotSchedule()
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
	if options.AutopilotCase, err = cmd.Flags().GetString("autopilot-case"); err != nil {
		return periodicOptions{}, err
	}
	if options.AutopilotRateMode, err = cmd.Flags().GetString("autopilot-rate-mode"); err != nil {
		return periodicOptions{}, err
	}
	if options.AutopilotBatchSamples, err = cmd.Flags().GetInt("autopilot-batch-samples"); err != nil {
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
		options.AutopilotSchedule = parseAutopilotSchedule(string(data), options.AutopilotSchedule)
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
	if !validAutopilotCase(options.AutopilotCase) {
		return periodicOptions{}, fmt.Errorf("autopilot-case must be repartition, migration, transient, or alternating")
	}
	if options.AutopilotRateMode != autopilotRateModeLegacy && options.AutopilotRateMode != autopilotRateModeStableRegion {
		return periodicOptions{}, fmt.Errorf("autopilot-rate-mode must be legacy or stable-region")
	}
	if options.AutopilotBatchSamples <= 0 {
		return periodicOptions{}, fmt.Errorf("autopilot-batch-samples must be greater than zero")
	}
	if options.AutopilotCase == "" && (!cmd.Flags().Changed("autopilot-expect") || (options.AutopilotExpect != "repartition" && options.AutopilotExpect != "rebalance" && options.AutopilotExpect != "both")) {
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
	if options.AutopilotCase == "transient" {
		options.BurstClass = burstClassTransient
	} else if options.AutopilotCase != "" {
		options.BurstClass = burstClassQualified
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

const (
	autopilotCaseAlternating = "alternating"
	autopilotCaseMigration   = "migration"
	autopilotCaseRepartition = "repartition"
	autopilotCaseTransient   = "transient"

	autopilotRateModeLegacy       = "legacy"
	autopilotRateModeStableRegion = "stable-region"
)

func validAutopilotCase(value string) bool {
	switch value {
	case "", autopilotCaseAlternating, autopilotCaseMigration, autopilotCaseRepartition, autopilotCaseTransient:
		return true
	default:
		return false
	}
}

func (o periodicOptions) usesStableRegionRateMode() bool {
	return o.AutopilotRateMode == autopilotRateModeStableRegion && o.AutopilotCase != ""
}

func (c burstClass) valid() bool {
	return c == burstClassMixed || c == burstClassTransient || c == burstClassQualified
}

func (c burstClass) expectation(configured string) string {
	if c == burstClassTransient {
		return "none"
	}
	return configured
}

func (o periodicOptions) expectedAutopilotAction(class burstClass) string {
	return expectedAutopilotAction(o.AutopilotCase, class, o.AutopilotExpect)
}

func expectedAutopilotAction(autopilotCase string, class burstClass, configured string) string {
	switch autopilotCase {
	case autopilotCaseRepartition:
		return "repartition"
	case autopilotCaseMigration:
		return "rebalance"
	case autopilotCaseTransient:
		return "none"
	default:
		return class.expectation(configured)
	}
}

func effectiveAutopilotCase(configured string, bundle *regionSnapshotBundle) string {
	if configured == autopilotCaseMigration && !migrationTargetReady(bundle) {
		return autopilotCaseRepartition
	}
	return configured
}

// autopilotCaseScheduler separates long-run case ordering from seeded target
// selection. Alternating workloads always attempt repartition then migration;
// if a migration still lacks a region surplus, its repartition bootstrap does
// not consume the pending migration turn.
type autopilotCaseScheduler struct {
	configured string
	next       string
	sequence   uint64
}

type autopilotCaseSelection struct {
	caseKind           string
	desiredCase        string
	nextCase           string
	sequence           uint64
	migrationBootstrap bool
}

func newAutopilotCaseScheduler(configured string) *autopilotCaseScheduler {
	scheduler := &autopilotCaseScheduler{configured: configured, next: configured}
	if configured == autopilotCaseAlternating {
		scheduler.next = autopilotCaseRepartition
	}
	return scheduler
}

func (s *autopilotCaseScheduler) selectCase(bundle *regionSnapshotBundle) autopilotCaseSelection {
	desired := s.next
	actual := effectiveAutopilotCase(desired, bundle)
	return autopilotCaseSelection{
		caseKind:           actual,
		desiredCase:        desired,
		sequence:           s.sequence + 1,
		migrationBootstrap: desired == autopilotCaseMigration && actual == autopilotCaseRepartition,
	}
}

func (s *autopilotCaseScheduler) advance(selection *autopilotCaseSelection, targetAvailable bool) {
	if s.configured != autopilotCaseAlternating || !targetAvailable {
		selection.nextCase = s.next
		return
	}
	// A bootstrap repartition is only preparation for the requested migration.
	// Retain that migration turn until a topology can actually select it.
	if selection.migrationBootstrap {
		s.next = autopilotCaseMigration
	} else if selection.desiredCase == autopilotCaseRepartition {
		s.next = autopilotCaseMigration
	} else {
		s.next = autopilotCaseRepartition
	}
	s.sequence = selection.sequence
	selection.nextCase = s.next
}

// parseAutopilotSchedule extracts the small set of timing knobs that are safe
// to read from either a MetaSrv TOML file or an embedded Helm config block.
// Unknown forms deliberately retain Enterprise defaults so a transient probe is
// conservative instead of assuming it can outlive the scheduler's history.
func parseAutopilotSchedule(snapshot string, defaults autopilotSchedule) autopilotSchedule {
	for _, line := range strings.Split(snapshot, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.Trim(strings.TrimSpace(value), "\"'")
		switch key {
		case "sampling_window":
			if duration, err := time.ParseDuration(value); err == nil && duration > 0 {
				defaults.samplingWindow = duration
			}
		case "max_history_windows":
			if windows, err := strconv.Atoi(value); err == nil && windows > 0 {
				defaults.maxHistoryWindows = windows
			}
		case "min_samples":
			if samples, err := strconv.Atoi(value); err == nil && samples > 0 {
				defaults.repartitionSamples = samples
			}
		case "max_region_history_cv":
			if cv, err := strconv.ParseFloat(value, 64); err == nil && cv > 0 {
				defaults.maxRegionHistoryCV = cv
			}
		case "min_write_samples":
			if samples, err := strconv.Atoi(value); err == nil && samples > 0 {
				defaults.migrationMinSamples = samples
			}
		case "write_window_stability_threshold", "window_stability_threshold":
			if windows, err := strconv.Atoi(value); err == nil && windows > 0 {
				defaults.writeStableWindows = windows
			}
		case "min_load_threshold":
			if bytes, ok := parseReadableBytes(value); ok && bytes > 0 {
				defaults.rebalanceMinLoadBPS = bytes
			}
		case "acceptable_load_ratio":
			if ratio, err := strconv.ParseFloat(value, 64); err == nil && ratio >= 0 {
				defaults.rebalanceAcceptableLoadRatio = ratio
			}
		}
	}
	return defaults
}

func parseReadableBytes(value string) (float64, bool) {
	normalized := strings.ToUpper(strings.TrimSpace(value))
	for _, unit := range []struct {
		suffix string
		bytes  float64
	}{
		{"MIB", 1024 * 1024},
		{"MB", 1024 * 1024},
		{"KIB", 1024},
		{"KB", 1024},
		{"B", 1},
	} {
		if !strings.HasSuffix(normalized, unit.suffix) {
			continue
		}
		amount, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimSuffix(normalized, unit.suffix)), 64)
		return amount * unit.bytes, err == nil
	}
	amount, err := strconv.ParseFloat(normalized, 64)
	return amount, err == nil
}

// stabilizationDuration is the minimum sustained plateau needed to replace
// the complete cluster-stat history after a workload level changes. Enterprise
// keeps one average per sampling window and rejects repartition when any
// leader region's retained history is unstable.
func (s autopilotSchedule) stabilizationDuration() time.Duration {
	windows := max(s.maxHistoryWindows, s.repartitionSamples)
	return time.Duration(windows+1+autopilotSettlingWindows) * s.samplingWindow
}

type burstPlan struct {
	class                 burstClass
	duration              time.Duration
	targetWriteBPS        float64
	maximumWriteBPS       float64
	pressureScale         float64
	schedulerBatchSamples int
}

func newBurstPlan(options periodicOptions, class burstClass, random *mathrand.Rand) burstPlan {
	if class == burstClassTransient {
		// Keep a transient below the source-derived history horizon. The final
		// observation window is left out deliberately: a probe must not collect
		// enough scheduler samples to become a stable scheduling candidate.
		requiredWindows := max(options.AutopilotSchedule.repartitionSamples, options.AutopilotSchedule.migrationMinSamples, options.AutopilotSchedule.writeStableWindows)
		maxDuration := time.Duration(max(requiredWindows-1, 1)) * options.AutopilotSchedule.samplingWindow
		maxDuration -= options.BaselineTickInterval
		if maxDuration < options.BaselineTickInterval {
			maxDuration = options.BaselineTickInterval
		}
		maxDuration = min(options.TransientBurstDuration, maxDuration)
		duration := options.BaselineTickInterval
		if maxDuration > duration {
			duration += time.Duration(random.Float64() * float64(maxDuration-duration))
		}
		scale := minTransientScale + random.Float64()*(maxTransientScale-minTransientScale)
		return burstPlan{
			class:           class,
			duration:        duration,
			targetWriteBPS:  options.PressureHighMinWriteBPS * scale,
			maximumWriteBPS: options.PressureHighMinWriteBPS * maxTransientScale,
			pressureScale:   scale,
		}
	}

	scale := minPressureScale + random.Float64()*(maxPressureScale-minPressureScale)
	duration := class.duration(options)
	if options.AutopilotCase != "" && class == burstClassQualified {
		duration = max(duration, options.AutopilotSchedule.stabilizationDuration())
	}
	return burstPlan{
		class:           class,
		duration:        duration,
		targetWriteBPS:  class.initialTargetBPS(options) * scale,
		maximumWriteBPS: options.QualifiedMaxWriteBPS * scale,
		pressureScale:   scale,
	}
}

// qualifyMigrationPlan derives the stable-mode client target from the
// Enterprise source-datanode threshold. The selected hot regions are
// co-located, but the all-region baseline is intentionally spread over the
// table, so leave a margin for the fraction of baseline that stays on the
// source node. The generic high-pressure target must not be retained here:
// remote-write bytes expand in region accounting, and starting at 1.5 times a
// generic pressure flag made the region-history CV exceed the 0.2 scheduler
// gate even while the client reported no errors.
func qualifyMigrationPlan(options periodicOptions, plan burstPlan, autopilotCase string) burstPlan {
	if autopilotCase != autopilotCaseMigration || plan.class != burstClassQualified {
		return plan
	}
	minimumTarget := options.AutopilotSchedule.rebalanceMinLoadBPS * (1 + options.AutopilotSchedule.rebalanceAcceptableLoadRatio + 0.2)
	if options.usesStableRegionRateMode() {
		scale := plan.pressureScale
		if scale <= 0 {
			scale = 1
		}
		plan.targetWriteBPS = minimumTarget * scale
		plan.maximumWriteBPS = minimumTarget * maxPressureScale
		return plan
	}
	if plan.targetWriteBPS < minimumTarget {
		plan.targetWriteBPS = minimumTarget
	}
	plan.maximumWriteBPS = max(plan.maximumWriteBPS, plan.targetWriteBPS)
	return plan
}

func qualifyRepartitionPlan(plan burstPlan, autopilotCase string) burstPlan {
	if autopilotCase != autopilotCaseRepartition || plan.class != burstClassQualified {
		return plan
	}
	// Retain the seeded per-cycle variation, but bound its upper edge so a
	// long-running case stays below the memtable flush horizon. This makes a
	// replay with the same seed reproducible without turning every qualified
	// repartition cycle into one identical fixed-rate plateau.
	scale := plan.pressureScale
	if scale <= 0 {
		scale = 1
	}
	plan.targetWriteBPS = min(plan.targetWriteBPS, stableRepartitionTargetBPS*scale)
	plan.maximumWriteBPS = min(plan.maximumWriteBPS, stableRepartitionTargetBPS*maxPressureScale)
	plan.schedulerBatchSamples = stableRepartitionBatchSamples
	return plan
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
	lane         trafficLane
	duration     time.Duration
	requestCount uint64
	sampleCount  uint64
	payloadBytes uint64
	err          error
}

type trafficLane string

const (
	baselineTrafficLane trafficLane = "baseline"
	hotspotTrafficLane  trafficLane = "hotspot"
)

type periodicWriteRequest struct {
	request prompb.WriteRequest
	lane    trafficLane
	pacer   *bytePacer
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
	// A partition value can represent only a small part of the sample-loader
	// configuration. Keep increasing its passes long enough for a qualified
	// burst to reach the configured pressure threshold instead of capping it at
	// a small multiple of the initial value.
	return &burstAmplifier{current: initial, max: max(initial*4, 256)}
}

func initialBurstAmplification(configured uint64, caseFocused bool) uint64 {
	if caseFocused {
		// A value-constrained pass produces only one partition-key value from
		// the configured sample set. Start with enough passes to fill the
		// global pacer in the first scheduler windows; otherwise the adaptive
		// loop records several low, unstable samples before it can reach the
		// requested pressure.
		return max(configured, uint64(minCaseBurstPasses))
	}
	return configured
}

func activeBaselineTarget(options periodicOptions, plan burstPlan, caseFocused bool) float64 {
	if !caseFocused {
		return options.BaselineTargetWriteBPS
	}
	// Keep every region's write-throughput history non-zero and stable while
	// reserving almost all of the active budget for the selected region. Auto
	// repartition rejects a table if any region history is unstable; making
	// non-target regions idle, or pacing them so slowly that their memtables
	// drain between heartbeats, causes their WCU history to fail that gate.
	return min(options.BaselineTargetWriteBPS, plan.targetWriteBPS*caseBaselineRatio)
}

// schedulerWorkerCount protects opt-in stable-region scheduler cases from a
// low user --workers value. Every retained background region needs a sender as
// well as every hot target value: sharing one baseline sender makes its region
// memtable-growth samples cycle and fail Enterprise's history-CV gate. The
// regular sample-loader path retains its exact configured worker count for
// backwards compatibility.
func schedulerWorkerCount(configured int, options periodicOptions, target *hotTarget, backgroundRegions int) int {
	if configured <= 0 || !options.usesStableRegionRateMode() || target == nil {
		return configured
	}
	return max(configured, max(backgroundRegions, 1)+len(stableTargetValues(*target))*stableTargetWorkers)
}

// pressureAdjustmentFloor limits adaptation for an explicit scheduler case to
// the configured high-pressure threshold. Chasing an aspirational client-side
// target after that threshold is met keeps changing the region load and makes
// the Enterprise stability gate reject the case indefinitely.
func pressureAdjustmentFloor(threshold, configuredTarget float64, caseFocused bool) float64 {
	if !caseFocused {
		return configuredTarget
	}
	return min(configuredTarget, threshold*1.1)
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

func periodicWorker(ctx context.Context, url, authorization, physicalTable string, requests <-chan periodicWriteRequest, results chan<- writeResult, wg *sync.WaitGroup, dryRun bool) {
	defer wg.Done()
	requester := benchhttp.NewRequester(url)
	if authorization != "" {
		requester.SetHeader("Authorization", authorization)
	}
	requester.SetHeader("x-greptime-hint-physical_table", physicalTable)
	for queued := range requests {
		result := writeResult{lane: queued.lane, requestCount: 1, sampleCount: sampleCount(queued.request)}
		started := time.Now()
		if dryRun {
			result.payloadBytes = 0
		} else {
			prepared, err := benchhttp.PrepareWriteRequest(queued.request)
			if err == nil {
				result.payloadBytes = uint64(prepared.PayloadBytes())
				err = queued.pacer.wait(ctx, prepared.PayloadBytes())
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

func enqueuePeriodicRequests(ctx context.Context, requests chan<- periodicWriteRequest, lane trafficLane, pacer *bytePacer, produce func(chan<- prompb.WriteRequest) bool) bool {
	raw := make(chan prompb.WriteRequest)
	produced := make(chan bool, 1)
	go func() {
		produced <- produce(raw)
		close(raw)
	}()
	for request := range raw {
		select {
		case requests <- periodicWriteRequest{request: request, lane: lane, pacer: pacer}:
		case <-ctx.Done():
			for range raw {
			}
			return false
		}
	}
	return <-produced
}

type workloadMetrics struct {
	requestCount uint64
	sampleCount  uint64
	payloadBytes uint64
	successCount uint64
	errorCount   uint64
	latencies    []time.Duration
	lastError    string
	baseline     trafficMetrics
	hotspot      trafficMetrics
}

type trafficMetrics struct {
	requestCount uint64
	sampleCount  uint64
	payloadBytes uint64
	successCount uint64
	errorCount   uint64
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
	lane := &c.metrics.baseline
	if result.lane == hotspotTrafficLane {
		lane = &c.metrics.hotspot
	}
	lane.requestCount += result.requestCount
	lane.sampleCount += result.sampleCount
	lane.payloadBytes += result.payloadBytes
	if result.err != nil {
		lane.errorCount++
	} else {
		lane.successCount++
	}
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
	baseEvent := benchmarkEvent{RunID: runID, TargetDatabase: options.TargetDatabase, LogicalTable: logicalTable, PhysicalTable: options.TargetPhysicalTable, AutopilotExpect: options.expectedAutopilotAction(options.BurstClass), AutopilotConfigHash: options.AutopilotConfigHash, AutopilotConfigSnapshot: options.AutopilotConfigSnapshot, AnalysisContextIncomplete: options.AutopilotConfigSnapshot == ""}
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
		refresher = &regionRefresher{client: client, physicalTable: options.TargetPhysicalTable, configTables: configTables, logicalTables: logicalTables, base: baseEvent, previous: make(map[uint64]regionSnapshot), rateHistory: make(map[uint64][]regionRateSample), schedule: options.AutopilotSchedule, rateMode: options.AutopilotRateMode, store: snapshots}
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
	caseScheduler := newAutopilotCaseScheduler(options.AutopilotCase)
	firstBurstAt := time.Now().Add(jitterDuration(options.BaselineDuration, options.BurstJitter, random))
	emitLifecycle("run_started", eventWith(baseEvent, "run_started", "baseline", 0, func(event *benchmarkEvent) {
		event.Details = fmt.Sprintf(`{"baseline_duration":"%s","baseline_tick_interval":"%s","burst_active_duration":"%s","transient_burst_duration":"%s","burst_period":"%s","burst_gap":"%s","burst_jitter":%g,"burst_amplification":%d,"burst_count":%d,"random_seed":%d,"observe_interval":"%s","periodic_traffic_mode":%q,"burst_class":%q,"autopilot_case":%q,"autopilot_rate_mode":%q,"autopilot_batch_samples":%d,"baseline_target_write_bps":%g,"qualified_max_write_bps":%g,"pressure_high_min_write_bps":%g,"scheduler_sampling_window":"%s","scheduler_history_windows":%d,"repartition_min_samples":%d,"max_region_history_cv":%g,"scheduler_stabilization_duration":"%s","configured_logical_tables":%s}`, options.BaselineDuration, options.BaselineTickInterval, options.BurstActiveDuration, options.TransientBurstDuration, options.BurstPeriod, options.BurstGap, options.BurstJitter, options.BurstAmplification, options.BurstCount, seed, options.ObserveInterval, options.TrafficMode, options.BurstClass, options.AutopilotCase, options.AutopilotRateMode, options.AutopilotBatchSamples, options.BaselineTargetWriteBPS, options.QualifiedMaxWriteBPS, options.PressureHighMinWriteBPS, options.AutopilotSchedule.samplingWindow, options.AutopilotSchedule.maxHistoryWindows, options.AutopilotSchedule.repartitionSamples, options.AutopilotSchedule.maxRegionHistoryCV, options.AutopilotSchedule.stabilizationDuration(), mustJSON(logicalTables))
	}), currentSnapshot(snapshots))
	for cycle := uint64(1); options.BurstCount == 0 || cycle <= options.BurstCount; cycle++ {
		class := classScheduler.next()
		plan := newBurstPlan(options, class, random)
		caseSelection := caseScheduler.selectCase(currentSnapshot(snapshots))
		caseKind := caseSelection.caseKind
		plan = qualifyMigrationPlan(options, plan, caseKind)
		plan = qualifyRepartitionPlan(plan, caseKind)
		plannedTarget, targetOK := selectHotTarget(currentSnapshot(snapshots), random, caseKind)
		caseScheduler.advance(&caseSelection, targetOK)
		if targetOK {
			log.Printf("periodic-burst: planned case=%s cycle=%d class=%s expect=%s regions=%v label=%s values=%d target_bps=%.0f duration=%s scale=%.3f start=%s", caseName(caseKind), cycle, plan.class, expectedAutopilotAction(caseKind, plan.class, options.AutopilotExpect), hotTargetRegionIDs(plannedTarget), plannedTarget.labelName, len(plannedTarget.values()), plan.targetWriteBPS, plan.duration, plan.pressureScale, firstBurstAt.UTC().Format(time.RFC3339))
		} else {
			log.Printf("periodic-burst: planned case=%s cycle=%d class=%s expect=%s without an eligible region target; start=%s", caseName(caseKind), cycle, plan.class, expectedAutopilotAction(caseKind, plan.class, options.AutopilotExpect), firstBurstAt.UTC().Format(time.RFC3339))
		}
		plannedWorkers := s.Workers
		if targetOK {
			plannedWorkers = schedulerWorkerCount(s.Workers, options, &plannedTarget, len(caseBackgroundPlan(fileConfigs, currentSnapshot(snapshots))))
		}
		plannedBatchSamples := effectiveSchedulerBatchSamples(s.MaxSamples, options, plan)
		emitLifecycle("pressure_scheduled", plannedBurstEvent(baseEvent, cycle, firstBurstAt, options, seed, caseSelection, plan, plannedTarget, targetOK, plannedWorkers, plannedBatchSamples, currentSnapshot(snapshots)), currentSnapshot(snapshots))
		if err := s.runPeriodicWrites(ctx, fileConfigs, &current, churnEpochGenerator, firstBurstAt, options.BaselineTickInterval, "baseline", 0, options, writer, snapshots, baseEvent, nil, burstPlan{class: burstClassTransient}); err != nil {
			return err
		}
		targetReselected := false
		previousTarget := plannedTarget
		if !hasHotTarget(currentSnapshot(snapshots), plannedTarget) {
			plannedTarget, targetOK = selectHotTarget(currentSnapshot(snapshots), random, caseKind)
			targetReselected = true
		}
		if writer != nil && targetReselected {
			emitLifecycle("pressure_target_reselected", eventWith(baseEvent, "pressure_target_reselected", "baseline", cycle, func(event *benchmarkEvent) {
				event.RegionID = plannedTarget.regionID
				event.Details = fmt.Sprintf(`{"reason":"partition_mapping_changed","previous_region_id":%d,"previous_label_name":%q,"previous_label_value":%q,"hot_label_name":%q,"hot_label_value":%q}`, previousTarget.regionID, previousTarget.labelName, previousTarget.labelValue, plannedTarget.labelName, plannedTarget.labelValue)
			}), currentSnapshot(snapshots))
		}
		var target *hotTarget
		if targetOK {
			target = &plannedTarget
		}
		effectiveWorkers := schedulerWorkerCount(s.Workers, options, target, len(caseBackgroundPlan(fileConfigs, currentSnapshot(snapshots))))
		effectiveBatchSamples := effectiveSchedulerBatchSamples(s.MaxSamples, options, plan)
		log.Printf("periodic-burst: starting case=%s cycle=%d class=%s expect=%s regions=%v workers=%d (configured=%d)", caseName(caseKind), cycle, plan.class, expectedAutopilotAction(caseKind, plan.class, options.AutopilotExpect), hotTargetRegionIDs(plannedTarget), effectiveWorkers, s.Workers)
		if targetOK && options.AutopilotCase != "" {
			backgrounds := caseBackgroundPlan(fileConfigs, currentSnapshot(snapshots))
			log.Printf("periodic-burst: scheduler baseline routes=%s", describeCaseBackgrounds(backgrounds))
		}
		if writer != nil {
			emitLifecycle("pressure_started", eventWith(baseEvent, "pressure_started", "active", cycle, func(event *benchmarkEvent) {
				now := time.Now()
				event.ScheduledTSMS = firstBurstAt.UnixMilli()
				event.ActualTSMS = now.UnixMilli()
				event.BurstClass = string(plan.class)
				event.AutopilotExpect = expectedAutopilotAction(caseKind, plan.class, options.AutopilotExpect)
				event.PressureThresholdBPS = floatEventValue(plan.targetWriteBPS)
				if targetOK {
					event.RegionID = plannedTarget.regionID
					event.Details = fmt.Sprintf(`{"hot_label_name":%q,"hot_label_values":%s,"selected_hot_label_values":%s,"split_input_label_values":%s,"hot_region_ids":%s,"burst_class":%q,"configured_autopilot_case":%q,"autopilot_case":%q,"desired_autopilot_case":%q,"next_autopilot_case":%q,"case_sequence":%d,"migration_bootstrap_repartition":%t,"autopilot_rate_mode":%q,"autopilot_batch_samples":%d,"effective_autopilot_batch_samples":%d,"configured_workers":%d,"effective_workers":%d,"burst_duration":"%s","pressure_scale":%g,"target_write_bps":%g}`, plannedTarget.labelName, mustJSON(plannedTarget.values()), mustJSON(selectedTargetLabelValues(plannedTarget)), mustJSON(selectedTargetValueSets(plannedTarget)), mustJSON(hotTargetRegionIDs(plannedTarget)), plan.class, options.AutopilotCase, caseKind, caseSelection.desiredCase, caseSelection.nextCase, caseSelection.sequence, caseSelection.migrationBootstrap, options.AutopilotRateMode, options.AutopilotBatchSamples, effectiveBatchSamples, s.Workers, effectiveWorkers, plan.duration, plan.pressureScale, plan.targetWriteBPS)
				}
			}), currentSnapshot(snapshots))
		}
		activeDeadline := time.Now().Add(plan.duration)
		if err := s.runPeriodicWrites(ctx, fileConfigs, &current, churnEpochGenerator, activeDeadline, s.TickInterval, "active", cycle, options, writer, snapshots, baseEvent, target, plan); err != nil {
			return err
		}
		log.Printf("periodic-burst: completed case=%s cycle=%d class=%s", caseName(caseKind), cycle, plan.class)
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

func caseName(autopilotCase string) string {
	if autopilotCase == "" {
		return "legacy"
	}
	return autopilotCase
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

var errPeriodicMappingChanged = errors.New("periodic workload partition mapping changed")

// runPeriodicWrites rebuilds isolated baseline lanes whenever the observer
// discovers that repartition or migration changed the live routing map. This
// keeps a long active case from writing a retired region set until the next
// scheduled case, while ordinary statistics refreshes continue uninterrupted.
func (s *SampleLoader) runPeriodicWrites(ctx context.Context, fileConfigs []samples.FileConfig, current *time.Time, churn *samples.ChurnEpochGenerator, deadline time.Time, tickInterval time.Duration, phase string, cycle uint64, options periodicOptions, writer *eventWriter, snapshots *regionSnapshotStore, base benchmarkEvent, target *hotTarget, plan burstPlan) error {
	if phase != "active" || target == nil || options.AutopilotCase == "" || snapshots == nil {
		return s.runPeriodicWritesEpoch(ctx, fileConfigs, current, churn, deadline, tickInterval, phase, cycle, options, writer, snapshots, base, target, plan)
	}

	for {
		bundle := currentSnapshot(snapshots)
		if bundle == nil {
			return s.runPeriodicWritesEpoch(ctx, fileConfigs, current, churn, deadline, tickInterval, phase, cycle, options, writer, snapshots, base, target, plan)
		}
		version := bundle.mappingVersion
		epochCtx, cancel := context.WithCancel(ctx)
		watcherStopped := make(chan struct{})
		mappingChanged := make(chan struct{})
		go func() {
			select {
			case <-snapshots.mappingChangeSignal(version):
				close(mappingChanged)
				cancel()
			case <-watcherStopped:
			}
		}()

		err := s.runPeriodicWritesEpoch(epochCtx, fileConfigs, current, churn, deadline, tickInterval, phase, cycle, options, writer, snapshots, base, target, plan)
		close(watcherStopped)
		cancel()
		select {
		case <-mappingChanged:
			if time.Now().Before(deadline) {
				latest := currentSnapshot(snapshots)
				if latest == nil {
					return errPeriodicMappingChanged
				}
				previousRegions := hotTargetRegionIDs(*target)
				if remapped, ok := remapHotTarget(latest, *target); ok {
					target = &remapped
				}
				log.Printf("periodic-burst: refreshed scheduler routes mapping_version=%d->%d regions=%v->%v", version, latest.mappingVersion, previousRegions, hotTargetRegionIDs(*target))
				if err := emitWithCurrentSnapshotAndFlush(writer, ctx, eventWith(base, "workload_routes_refreshed", phase, cycle, func(event *benchmarkEvent) {
					event.RegionID = target.regionID
					event.Details = fmt.Sprintf(`{"reason":"partition_mapping_changed","previous_mapping_version":%d,"current_mapping_version":%d,"hot_region_ids":%s,"selected_hot_label_values":%s}`, version, latest.mappingVersion, mustJSON(hotTargetRegionIDs(*target)), mustJSON(selectedTargetLabelValues(*target)))
				}), snapshots); err != nil {
					log.Printf("periodic-burst: flush workload_routes_refreshed event: %v", err)
				}
				continue
			}
			return nil
		default:
		}
		if err == context.Canceled && ctx.Err() == nil {
			return errPeriodicMappingChanged
		}
		return err
	}
}

func (s *SampleLoader) runPeriodicWritesEpoch(ctx context.Context, fileConfigs []samples.FileConfig, current *time.Time, churn *samples.ChurnEpochGenerator, deadline time.Time, tickInterval time.Duration, phase string, cycle uint64, options periodicOptions, writer *eventWriter, snapshots *regionSnapshotStore, base benchmarkEvent, target *hotTarget, plan burstPlan) error {
	phaseCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	caseFocused := phase == "active" && target != nil && options.AutopilotCase != ""
	backgrounds := []caseBackground(nil)
	if caseFocused {
		backgrounds = caseBackgroundPlan(fileConfigs, currentSnapshot(snapshots))
	}
	backgroundRegions := len(backgrounds)
	effectiveWorkers := schedulerWorkerCount(s.Workers, options, target, backgroundRegions)
	requests := make(chan periodicWriteRequest, effectiveWorkers)
	baselineRequests := requests
	dedicatedBaselineLane := caseFocused && effectiveWorkers > 1
	if dedicatedBaselineLane {
		baselineRequests = make(chan periodicWriteRequest, 1)
	}
	results := make(chan writeResult, effectiveWorkers*2)
	collector := &workloadCollector{}
	collectorDone := make(chan struct{})
	go func() {
		defer close(collectorDone)
		for result := range results {
			collector.add(result)
		}
	}()

	var workers sync.WaitGroup
	var baselinePacer *bytePacer
	var hotspotPacer *bytePacer
	var amplifier *burstAmplifier
	if options.TrafficMode == "steady" {
		// An explicit scheduler case makes one selected region observably hotter
		// than the rest of the table. It keeps a deliberately small global
		// baseline so every region has stable history for the Enterprise
		// baseline gate.
		baselinePacer = newBytePacer(activeBaselineTarget(options, plan, caseFocused))
		if phase == "active" && target != nil {
			hotspotTarget := max(plan.targetWriteBPS-baselinePacer.target(), 0)
			hotspotPacer = newBytePacer(hotspotTarget)
			amplifier = newBurstAmplifier(initialBurstAmplification(options.BurstAmplification, caseFocused))
		}
	}
	schedulerLoader := s
	if options.usesStableRegionRateMode() {
		// Scheduler history is sampled from region memtable growth. Bound the
		// request batch even when the ordinary sample-loader default is large so
		// global payload pacing reaches the datanode as a stream, rather than as
		// alternating full-pass memtable bursts. The opt-in mode never expands a
		// caller's existing --max-samples limit.
		stableLoader := *s
		stableLoader.MaxSamples = effectiveSchedulerBatchSamples(s.MaxSamples, options, plan)
		schedulerLoader = &stableLoader
	}
	baselineWorkers := 0
	baselineRequestsByRegion := make(map[uint64]chan periodicWriteRequest)
	baselineRequestFor := func(regionID uint64) chan periodicWriteRequest {
		if requests, ok := baselineRequestsByRegion[regionID]; ok {
			return requests
		}
		return baselineRequests
	}
	if dedicatedBaselineLane {
		baselineWorkers = 1
		if options.usesStableRegionRateMode() {
			// A focused scheduler baseline needs an isolated producer/worker pair
			// for each region. Sending all regions through one channel serializes
			// their multi-request config passes: although each pass has its own
			// byte pacer, another region waits behind it and its heartbeat samples
			// alternate between a full and partial interval.
			for _, background := range backgrounds {
				requests := make(chan periodicWriteRequest, 1)
				baselineRequestsByRegion[background.target.regionID] = requests
				workers.Add(1)
				go periodicWorker(ctx, s.RemoteWriteURL, s.authorizationHeader(), options.TargetPhysicalTable, requests, results, &workers, s.DryRun)
			}
			baselineWorkers = len(baselineRequestsByRegion)
		}
		if len(baselineRequestsByRegion) == 0 {
			for range baselineWorkers {
				workers.Add(1)
				go periodicWorker(ctx, s.RemoteWriteURL, s.authorizationHeader(), options.TargetPhysicalTable, baselineRequests, results, &workers, s.DryRun)
			}
		}
	}
	hotspotWorkers := effectiveWorkers
	if dedicatedBaselineLane {
		hotspotWorkers -= baselineWorkers
	}
	for range hotspotWorkers {
		workers.Add(1)
		go periodicWorker(ctx, s.RemoteWriteURL, s.authorizationHeader(), options.TargetPhysicalTable, requests, results, &workers, s.DryRun)
	}

	started := time.Now()
	// Each current region gets an independent pacer. Sharing one pacer makes a
	// large representative config delay the other regions, producing sparse,
	// uneven heartbeat samples that the Enterprise baseline gate rejects.
	backgroundPacers := make(map[uint64]*bytePacer)
	for _, background := range backgrounds {
		backgroundPacers[background.target.regionID] = newBytePacer(baselinePacer.target() / float64(backgroundRegions))
	}
	backgroundPacer := func(regionID uint64, regionCount int) *bytePacer {
		if baselinePacer == nil || regionCount == 0 {
			return nil
		}
		if pacer := backgroundPacers[regionID]; pacer != nil {
			return pacer
		}
		pacer := newBytePacer(baselinePacer.target() / float64(regionCount))
		backgroundPacers[regionID] = pacer
		return pacer
	}
	// Stable-region mode keeps a deterministic, balanced value sequence while
	// using the one hotspot pacer for the aggregate target. Applying separate
	// pacers to serially generated passes under-delivers the total rate and
	// produces alternating server-side memtable deltas.
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
		if dedicatedBaselineLane {
			if len(baselineRequestsByRegion) == 0 {
				defer close(baselineRequests)
			} else {
				defer func() {
					for _, requests := range baselineRequestsByRegion {
						close(requests)
					}
				}()
			}
		}
		if caseFocused && dedicatedBaselineLane {
			// The target generator can take longer than one scrape interval to
			// build a high-cardinality hot pass. Keep the all-region baseline in a
			// separate producer as well as a separate worker; otherwise it emits
			// only once before the hot pass monopolizes this loop and its regions
			// decay to zero in MetaSrv heartbeat history.
			var currentMu sync.Mutex
			nextTimestamp := func() time.Time {
				currentMu.Lock()
				defer currentMu.Unlock()
				// A byte-paced scheduler case can issue many batches in one scrape
				// interval. Keep those samples close to wall-clock time instead of
				// advancing the synthetic timestamp by one scrape per batch: doing
				// the latter quickly writes hours into the future once the pacer is
				// able to sustain the requested rate.
				now := time.Now()
				if now.After(*current) {
					*current = now
				}
				timestamp := *current
				*current = current.Add(time.Millisecond)
				return timestamp
			}
			var generators sync.WaitGroup
			stableValues := stableTargetValues(*target)
			// Keep all regions on the dedicated baseline lane even when a
			// migration starts one producer per hot region below. The balancer
			// marks a datanode ineligible when too much of its region load is
			// unknown or unstable; omitting this producer leaves the non-hot
			// datanodes at zero and prevents an otherwise valid move.
			for _, background := range backgrounds {
				background := background
				values := background.target.values()
				if len(values) == 0 {
					continue
				}
				pacer := backgroundPacer(background.target.regionID, len(backgrounds))
				requests := baselineRequestFor(background.target.regionID)
				generators.Add(1)
				go func() {
					defer generators.Done()
					for {
						epoch := churn.GetChurnEpoch()
						timestamp := nextTimestamp()
						if !enqueuePeriodicRequests(phaseCtx, requests, baselineTrafficLane, pacer, func(raw chan<- prompb.WriteRequest) bool {
							return schedulerLoader.convertToRemoteWriteRequestsStreamingWithLabelValues(phaseCtx, []samples.FileConfig{background.fileConfig}, timestamp, raw, epoch, map[string]string{background.target.labelName: values[0]}, nil)
						}) {
							return
						}
						// The byte pacer, rather than the scrape ticker, keeps the
						// region's memtable growth continuous at its assigned rate.
						if phaseCtx.Err() != nil {
							return
						}
					}
				}()
			}
			if options.usesStableRegionRateMode() && len(stableValues) > 1 {
				// Migration heats several co-located regions, while repartition heats
				// several fixed values inside one region. Feed each value from an
				// independent producer and pacer: serial alternation makes a
				// region's memtable delta oscillate even when aggregate client BPS is
				// constant.
				// The copies avoid sharing FileConfig's mutable field-generator cache.
				for _, valueTarget := range stableValues {
					valueTarget := valueTarget
					configs := schedulerCaseFileConfigs(fileConfigs, valueTarget.target, valueTarget.value)
					if len(configs) == 0 {
						continue
					}
					pacer := newBytePacer(hotspotPacer.target() / float64(len(stableValues)))
					generators.Add(1)
					go func() {
						defer generators.Done()
						for {
							epoch := churn.GetChurnEpoch()
							timestamp := nextTimestamp()
							if !enqueuePeriodicRequests(phaseCtx, requests, hotspotTrafficLane, pacer, func(raw chan<- prompb.WriteRequest) bool {
								return schedulerLoader.convertToRemoteWriteRequestsStreamingWithLabelValues(phaseCtx, configs, timestamp, raw, epoch, map[string]string{valueTarget.target.labelName: valueTarget.value}, nil)
							}) {
								return
							}
							// A producer stays runnable until its byte pacer blocks a
							// worker. This lets the configured pacer, not --interval,
							// deliver the requested per-region pressure.
							if phaseCtx.Err() != nil {
								return
							}
						}
					}()
				}
			} else {
				generators.Add(1)
				go func() {
					defer generators.Done()
					tick := time.NewTicker(tickInterval)
					defer tick.Stop()
					for {
						epoch := churn.GetChurnEpoch()
						targets := target.workloadTargets()
						if len(targets) == 0 {
							return
						}
						if options.usesStableRegionRateMode() {
							for _, valueTarget := range stableTargetValuePasses(*target, amplifier.value()) {
								configs := schedulerCaseFileConfigs(fileConfigs, valueTarget.target, valueTarget.value)
								if len(configs) == 0 {
									continue
								}
								timestamp := nextTimestamp()
								if !enqueuePeriodicRequests(phaseCtx, requests, hotspotTrafficLane, hotspotPacer, func(raw chan<- prompb.WriteRequest) bool {
									return schedulerLoader.convertToRemoteWriteRequestsStreamingWithLabelValues(phaseCtx, configs, timestamp, raw, epoch, map[string]string{valueTarget.target.labelName: valueTarget.value}, nil)
								}) {
									return
								}
							}
						} else {
							for pass := range amplifier.value() {
								selectedTarget := targets[pass%uint64(len(targets))]
								values := selectedTarget.values()
								if len(values) == 0 {
									continue
								}
								labelValue := values[(pass/uint64(len(targets)))%uint64(len(values))]
								timestamp := nextTimestamp()
								if !enqueuePeriodicRequests(phaseCtx, requests, hotspotTrafficLane, hotspotPacer, func(raw chan<- prompb.WriteRequest) bool {
									return s.convertToRemoteWriteRequestsStreamingWithLabelValues(phaseCtx, fileConfigs, timestamp, raw, epoch, map[string]string{selectedTarget.labelName: labelValue}, nil)
								}) {
									return
								}
							}
						}
						select {
						case <-phaseCtx.Done():
							return
						case <-tick.C:
						}
					}
				}()
			}
			generators.Wait()
			return
		}
		tick := time.NewTicker(tickInterval)
		defer tick.Stop()
		generate := func() {
			epoch := churn.GetChurnEpoch()
			if options.TrafficMode != "steady" {
				repetitions := uint64(1)
				if target != nil {
					repetitions = options.BurstAmplification
				}
				for range repetitions {
					if !enqueuePeriodicRequests(phaseCtx, requests, baselineTrafficLane, nil, func(raw chan<- prompb.WriteRequest) bool {
						if target == nil {
							return s.convertToRemoteWriteRequestsStreaming(phaseCtx, fileConfigs, *current, raw, epoch)
						}
						return s.convertToRemoteWriteRequestsStreamingFiltered(phaseCtx, fileConfigs, *current, raw, epoch, func(series prompb.TimeSeries) bool {
							return matchesHotTarget(series, *target)
						})
					}) {
						return
					}
					*current = current.Add(s.Interval)
				}
				return
			}
			if target == nil {
				enqueuePeriodicRequests(phaseCtx, requests, baselineTrafficLane, baselinePacer, func(raw chan<- prompb.WriteRequest) bool {
					return s.convertToRemoteWriteRequestsStreaming(phaseCtx, fileConfigs, *current, raw, epoch)
				})
				*current = current.Add(s.Interval)
				return
			}

			if caseFocused {
				backgrounds := caseBackgroundPlan(fileConfigs, currentSnapshot(snapshots))
				for _, background := range backgrounds {
					values := background.target.values()
					if len(values) == 0 {
						continue
					}
					pacer := backgroundPacer(background.target.regionID, len(backgrounds))
					if !enqueuePeriodicRequests(phaseCtx, baselineRequests, baselineTrafficLane, pacer, func(raw chan<- prompb.WriteRequest) bool {
						return schedulerLoader.convertToRemoteWriteRequestsStreamingWithLabelValues(phaseCtx, []samples.FileConfig{background.fileConfig}, *current, raw, epoch, map[string]string{background.target.labelName: values[0]}, nil)
					}) {
						return
					}
				}
			} else if !enqueuePeriodicRequests(phaseCtx, baselineRequests, baselineTrafficLane, baselinePacer, func(raw chan<- prompb.WriteRequest) bool {
				return s.convertToRemoteWriteRequestsStreaming(phaseCtx, fileConfigs, *current, raw, epoch)
			}) {
				return
			}
			*current = current.Add(s.Interval)

			// The active phase keeps the normal config-derived baseline and adds a
			// target-region lane. An explicit case uses a representative config
			// sample for its low baseline so that it does not delay this lane;
			// legacy/static expectations retain the full configured baseline.
			// Cycling values makes repartition input span the region's partition
			// key range instead of concentrating every new row on one identical key.
			if hotspotPacer == nil {
				return
			}
			targets := target.workloadTargets()
			if len(targets) == 0 {
				return
			}
			if options.usesStableRegionRateMode() {
				for _, valueTarget := range stableTargetValuePasses(*target, amplifier.value()) {
					if !enqueuePeriodicRequests(phaseCtx, requests, hotspotTrafficLane, hotspotPacer, func(raw chan<- prompb.WriteRequest) bool {
						return schedulerLoader.convertToRemoteWriteRequestsStreamingWithLabelValues(phaseCtx, fileConfigs, *current, raw, epoch, map[string]string{valueTarget.target.labelName: valueTarget.value}, nil)
					}) {
						return
					}
					*current = current.Add(s.Interval)
				}
			} else {
				repetitions := amplifier.value()
				for pass := range repetitions {
					selectedTarget := targets[pass%uint64(len(targets))]
					values := selectedTarget.values()
					if len(values) == 0 {
						continue
					}
					labelValue := values[(pass/uint64(len(targets)))%uint64(len(values))]
					if !enqueuePeriodicRequests(phaseCtx, requests, hotspotTrafficLane, hotspotPacer, func(raw chan<- prompb.WriteRequest) bool {
						return s.convertToRemoteWriteRequestsStreamingWithLabelValues(phaseCtx, fileConfigs, *current, raw, epoch, map[string]string{selectedTarget.labelName: labelValue}, nil)
					}) {
						return
					}
					*current = current.Add(s.Interval)
				}
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
			emitWorkloadWindow(ctx, writer, snapshots, base, phase, cycle, windowStarted, now, collector.reset(), options, consecutiveHigh, baselinePacer, hotspotPacer, amplifier, plan, caseFocused)
			return nil
		case now := <-window.C:
			consecutiveHigh = emitWorkloadWindow(ctx, writer, snapshots, base, phase, cycle, windowStarted, now, collector.reset(), options, consecutiveHigh, baselinePacer, hotspotPacer, amplifier, plan, caseFocused)
			windowStarted = now
		}
	}
}

func emitWorkloadWindow(ctx context.Context, writer *eventWriter, snapshots *regionSnapshotStore, base benchmarkEvent, phase string, cycle uint64, start, end time.Time, metrics workloadMetrics, options periodicOptions, consecutiveHigh uint64, baselinePacer, hotspotPacer *bytePacer, amplifier *burstAmplifier, plan burstPlan, caseFocused bool) uint64 {
	if writer == nil || !end.After(start) {
		return consecutiveHigh
	}
	threshold := options.PressureHighMinWriteBPS
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
		event.BurstClass = string(plan.class)
		baselineTarget := baselinePacer.target()
		hotspotTarget := hotspotPacer.target()
		if baselineTarget > 0 || hotspotTarget > 0 {
			event.Details = fmt.Sprintf(`{"target_write_bps":%g,"baseline_target_write_bps":%g,"hotspot_target_write_bps":%g,"baseline_write_bps":%g,"hotspot_write_bps":%g,"source_passes":%d,"autopilot_rate_mode":%q}`, baselineTarget+hotspotTarget, baselineTarget, hotspotTarget, float64(metrics.baseline.payloadBytes)/duration.Seconds(), float64(metrics.hotspot.payloadBytes)/duration.Seconds(), amplifier.value(), options.AutopilotRateMode)
		}
	})
	if writeBPS >= threshold && metrics.errorCount == 0 {
		consecutiveHigh++
	} else {
		if consecutiveHigh >= 3 {
			if err := emitWithCurrentSnapshotAndFlush(writer, ctx, eventWith(base, "pressure_dropped", phase, cycle, func(event *benchmarkEvent) {
				event.WriteBPS = floatEventValue(writeBPS)
				event.PressureThresholdBPS = floatEventValue(threshold)
				event.ConsecutiveHighWindows = consecutiveHigh
			}), snapshots); err != nil {
				log.Printf("periodic-burst: flush pressure_dropped event: %v", err)
			}
		}
		consecutiveHigh = 0
	}
	event.ConsecutiveHighWindows = consecutiveHigh
	if err := emitWithCurrentSnapshotAndFlush(writer, ctx, event, snapshots); err != nil {
		log.Printf("periodic-burst: flush workload_window event: %v", err)
	}
	if metrics.errorCount > 0 {
		if err := emitWithCurrentSnapshotAndFlush(writer, ctx, eventWith(base, "write_error", phase, cycle, func(event *benchmarkEvent) {
			event.WindowStartMS = start.UnixMilli()
			event.WindowEndMS = end.UnixMilli()
			event.ErrorCount = metrics.errorCount
			event.Error = metrics.lastError
		}), snapshots); err != nil {
			log.Printf("periodic-burst: flush write_error event: %v", err)
		}
	}
	if consecutiveHigh == 3 {
		if err := emitWithCurrentSnapshotAndFlush(writer, ctx, eventWith(base, "pressure_observed_high", phase, cycle, func(event *benchmarkEvent) {
			event.WriteBPS = floatEventValue(writeBPS)
			event.PressureThresholdBPS = floatEventValue(threshold)
			event.ConsecutiveHighWindows = consecutiveHigh
		}), snapshots); err != nil {
			log.Printf("periodic-burst: flush pressure_observed_high event: %v", err)
		}
	}
	adjustmentFloor := pressureAdjustmentFloor(threshold, baselinePacer.target()+hotspotPacer.target(), caseFocused)
	if adaptivePressureEnabled(options, phase, plan, metrics, hotspotPacer) && writeBPS < adjustmentFloor*0.9 {
		if hotspotTarget, increased := hotspotPacer.increase(max(plan.maximumWriteBPS-baselinePacer.target(), 0)); increased {
			sourcePasses, _ := amplifier.increase()
			if err := emitWithCurrentSnapshotAndFlush(writer, ctx, eventWith(base, "pressure_rate_adjusted", phase, cycle, func(event *benchmarkEvent) {
				event.BurstClass = string(plan.class)
				event.WriteBPS = floatEventValue(writeBPS)
				event.PressureThresholdBPS = floatEventValue(baselinePacer.target() + hotspotTarget)
				event.Details = fmt.Sprintf(`{"reason":"observed_below_target","new_target_write_bps":%g,"baseline_target_write_bps":%g,"hotspot_target_write_bps":%g,"max_target_write_bps":%g,"pressure_scale":%g,"source_passes":%d}`, baselinePacer.target()+hotspotTarget, baselinePacer.target(), hotspotTarget, plan.maximumWriteBPS, plan.pressureScale, sourcePasses)
			}), snapshots); err != nil {
				log.Printf("periodic-burst: flush pressure_rate_adjusted event: %v", err)
			}
		}
	}
	return consecutiveHigh
}

func adaptivePressureEnabled(options periodicOptions, phase string, plan burstPlan, metrics workloadMetrics, hotspotPacer *bytePacer) bool {
	return !options.usesStableRegionRateMode() && phase == "active" && plan.class == burstClassQualified && metrics.errorCount == 0 && hotspotPacer != nil
}

func effectiveSchedulerBatchSamples(configuredMaxSamples int, options periodicOptions, plan burstPlan) int {
	if !options.usesStableRegionRateMode() {
		return configuredMaxSamples
	}
	return stableRegionBatchSamples(configuredMaxSamples, options.AutopilotBatchSamples, plan.schedulerBatchSamples)
}

func stableRegionBatchSamples(configuredMaxSamples, autopilotBatchSamples, schedulerBatchSamples int) int {
	if schedulerBatchSamples > 0 {
		autopilotBatchSamples = min(autopilotBatchSamples, schedulerBatchSamples)
	}
	return min(configuredMaxSamples, autopilotBatchSamples)
}

func emitWithCurrentSnapshot(writer *eventWriter, ctx context.Context, event benchmarkEvent, snapshots *regionSnapshotStore) {
	if snapshots == nil {
		writer.emit(ctx, event)
		return
	}
	writer.emitWithSnapshot(ctx, event, snapshots.get())
}

// emitWithCurrentSnapshotAndFlush persists high-signal workload evidence on
// the same cadence that it is observed. Unlike lifecycle emission it does not
// replay every region-stat event already attached to the snapshot; this keeps
// the self-monitoring stream compact while letting Grafana and agents inspect
// a live case before it completes.
func emitWithCurrentSnapshotAndFlush(writer *eventWriter, ctx context.Context, event benchmarkEvent, snapshots *regionSnapshotStore) error {
	if snapshots == nil {
		return writer.emitAndFlush(ctx, event)
	}
	bundle := snapshots.get()
	if bundle == nil {
		return writer.emitAndFlush(ctx, event)
	}
	writer.mu.Lock()
	defer writer.mu.Unlock()
	writer.appendLocked(withSnapshotDetails(event, bundle))
	return writer.flushLocked(ctx)
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

func plannedBurstEvent(base benchmarkEvent, cycle uint64, scheduled time.Time, options periodicOptions, seed int64, selection autopilotCaseSelection, plan burstPlan, target hotTarget, targetOK bool, effectiveWorkers, effectiveBatchSamples int, snapshot *regionSnapshotBundle) benchmarkEvent {
	return eventWith(base, "pressure_scheduled", "baseline", cycle, func(event *benchmarkEvent) {
		event.ScheduledTSMS = scheduled.UnixMilli()
		event.BurstClass = string(plan.class)
		event.AutopilotExpect = expectedAutopilotAction(selection.caseKind, plan.class, options.AutopilotExpect)
		event.PressureThresholdBPS = floatEventValue(plan.targetWriteBPS)
		topology := rebalanceTopology(snapshot)
		if targetOK {
			event.RegionID = target.regionID
			splitInputReady := selection.caseKind != autopilotCaseRepartition || len(stableTargetValues(target)) >= 2
			event.Details = fmt.Sprintf(`{"random_seed":%d,"burst_class":%q,"configured_autopilot_case":%q,"autopilot_case":%q,"desired_autopilot_case":%q,"next_autopilot_case":%q,"case_sequence":%d,"autopilot_rate_mode":%q,"autopilot_batch_samples":%d,"effective_autopilot_batch_samples":%d,"effective_workers":%d,"migration_bootstrap_repartition":%t,"burst_duration":"%s","burst_period":"%s","burst_gap":"%s","burst_jitter":%g,"burst_amplification":%d,"pressure_scale":%g,"target_write_bps":%g,"max_target_write_bps":%g,"hot_label_name":%q,"hot_label_values":%s,"selected_hot_label_values":%s,"split_input_label_values":%s,"hot_label_value_count":%d,"repartition_split_input_ready":%t,"autopilot_sampling_window":"%s","repartition_min_samples":%d,"max_region_history_cv":%g,"migration_min_samples":%d,"migration_stable_windows":%d,"rebalance_min_load_bps":%g,"rebalance_acceptable_load_ratio":%g,"available_region_count":%d,"observed_leader_datanode_count":%d,"rebalance_region_surplus":%d,"rebalance_topology_ready":%t}`, seed, plan.class, options.AutopilotCase, selection.caseKind, selection.desiredCase, selection.nextCase, selection.sequence, options.AutopilotRateMode, options.AutopilotBatchSamples, effectiveBatchSamples, effectiveWorkers, selection.migrationBootstrap, plan.duration, options.BurstPeriod, options.BurstGap, options.BurstJitter, options.BurstAmplification, plan.pressureScale, plan.targetWriteBPS, plan.maximumWriteBPS, target.labelName, mustJSON(target.values()), mustJSON(selectedTargetLabelValues(target)), mustJSON(selectedTargetValueSets(target)), len(target.values()), splitInputReady, options.AutopilotSchedule.samplingWindow, options.AutopilotSchedule.repartitionSamples, options.AutopilotSchedule.maxRegionHistoryCV, options.AutopilotSchedule.migrationMinSamples, options.AutopilotSchedule.writeStableWindows, options.AutopilotSchedule.rebalanceMinLoadBPS, options.AutopilotSchedule.rebalanceAcceptableLoadRatio, topology.regionCount, topology.datanodeCount, topology.regionSurplus, topology.ready)
		} else {
			event.Details = fmt.Sprintf(`{"random_seed":%d,"burst_class":%q,"configured_autopilot_case":%q,"autopilot_case":%q,"desired_autopilot_case":%q,"next_autopilot_case":%q,"case_sequence":%d,"effective_workers":%d,"migration_bootstrap_repartition":%t,"burst_duration":"%s","burst_period":"%s","burst_gap":"%s","burst_jitter":%g,"burst_amplification":%d,"pressure_scale":%g,"target_write_bps":%g,"max_target_write_bps":%g,"hot_target_unavailable":true,"available_region_count":%d,"observed_leader_datanode_count":%d,"rebalance_region_surplus":%d,"rebalance_topology_ready":%t}`, seed, plan.class, options.AutopilotCase, selection.caseKind, selection.desiredCase, selection.nextCase, selection.sequence, effectiveWorkers, selection.migrationBootstrap, plan.duration, options.BurstPeriod, options.BurstGap, options.BurstJitter, options.BurstAmplification, plan.pressureScale, plan.targetWriteBPS, plan.maximumWriteBPS, topology.regionCount, topology.datanodeCount, topology.regionSurplus, topology.ready)
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

type regionRateSample struct {
	timestamp time.Time
	writeBPS  float64
}

type regionSnapshotBundle struct {
	id                 string
	timestamp          time.Time
	mappingVersion     uint64
	mappingFingerprint string
	snapshots          []regionSnapshot
	events             []benchmarkEvent
	distribution       []sharedpartition.RegionDistribution
	logicalTables      []string
	hotTargets         []hotTarget
	datanodeCount      uint64
	refreshError       string
	stale              bool
}

type rebalanceTopologySummary struct {
	regionCount   uint64
	datanodeCount uint64
	regionSurplus int64
	ready         bool
}

// rebalanceTopology summarizes the live region snapshot without adding new
// typed self-monitoring columns. A rebalance needs more movable regions than
// observed datanodes; the summary lets later analysis distinguish an
// insufficient topology from a traffic or scheduler failure.
func rebalanceTopology(bundle *regionSnapshotBundle) rebalanceTopologySummary {
	if bundle == nil {
		return rebalanceTopologySummary{}
	}
	leaders := make(map[string]struct{})
	seenRegions := make(map[uint64]struct{})
	for _, snapshot := range bundle.snapshots {
		if snapshot.regionID == 0 {
			continue
		}
		seenRegions[snapshot.regionID] = struct{}{}
		if snapshot.leader != "" {
			leaders[snapshot.leader] = struct{}{}
		}
	}
	regionCount := uint64(len(seenRegions))
	datanodeCount := bundle.datanodeCount
	if datanodeCount == 0 {
		datanodeCount = uint64(len(leaders))
	}
	surplus := int64(regionCount) - int64(datanodeCount)
	return rebalanceTopologySummary{
		regionCount:   regionCount,
		datanodeCount: datanodeCount,
		regionSurplus: surplus,
		ready:         datanodeCount > 0 && regionCount > datanodeCount,
	}
}

// hotTarget is a config-derived label-value set that routes writes to one current region.
// Repartition needs multiple values within the region so the scheduler can sample
// a real split boundary instead of repeatedly observing one identical key.
type hotTarget struct {
	regionID    uint64
	labelName   string
	labelValue  string
	labelValues []string
	// caseValues is the deterministic subset written during one focused case.
	// It differs from labelValues, which contains every config-derived value
	// currently mapped to the region. Repartition needs multiple, spread-out
	// values for its split-point sampler; migration deliberately uses one
	// equal-shaped value per region.
	caseValues []string
	// members is populated only for a migration case.  Region balancing must
	// move one reasonably sized region off an already hot datanode; making a
	// single region overwhelmingly hot cannot improve the balance after moving
	// it.  Keep the public planning shape as one hotTarget while carrying the
	// colocated regions that form the migration workload.
	members []hotTarget
}

func (t hotTarget) values() []string {
	if len(t.labelValues) != 0 {
		return t.labelValues
	}
	if t.labelValue == "" {
		return nil
	}
	return []string{t.labelValue}
}

func (t hotTarget) workloadTargets() []hotTarget {
	if len(t.members) != 0 {
		return t.members
	}
	return []hotTarget{{
		regionID:    t.regionID,
		labelName:   t.labelName,
		labelValue:  t.labelValue,
		labelValues: append([]string(nil), t.labelValues...),
		caseValues:  append([]string(nil), t.caseValues...),
	}}
}

func hotTargetRegionIDs(target hotTarget) []uint64 {
	targets := target.workloadTargets()
	ids := make([]uint64, 0, len(targets))
	for _, item := range targets {
		ids = append(ids, item.regionID)
	}
	return ids
}

type targetValuePass struct {
	target hotTarget
	value  string
}

// stableTargetValuePasses repeats the deterministic per-case value set for
// the duration of an active case. Each selected value uses the same fixed
// representative config, so repartition can cover its partition range without
// reintroducing shape changes into MetaSrv's 45-second throughput history.
func stableTargetValuePasses(target hotTarget, passBudget uint64) []targetValuePass {
	values := stableTargetValues(target)
	if len(values) == 0 {
		return nil
	}
	passes := max(passBudget, uint64(len(values)))
	result := make([]targetValuePass, 0, passes)
	for pass := uint64(0); pass < passes; pass++ {
		result = append(result, values[pass%uint64(len(values))])
	}
	return result
}

func stableTargetValues(target hotTarget) []targetValuePass {
	values := make([]targetValuePass, 0)
	for _, item := range target.workloadTargets() {
		caseValues := item.caseValues
		if len(caseValues) == 0 && item.labelValue != "" {
			caseValues = []string{item.labelValue}
		}
		if len(caseValues) == 0 {
			itemValues := item.values()
			if len(itemValues) == 0 {
				continue
			}
			caseValues = []string{itemValues[0]}
		}
		for _, value := range caseValues {
			values = append(values, targetValuePass{target: item, value: value})
		}
	}
	return values
}

func selectedTargetLabelValues(target hotTarget) map[uint64]string {
	selected := make(map[uint64]string)
	for _, value := range stableTargetValues(target) {
		selected[value.target.regionID] = value.value
	}
	return selected
}

func selectedTargetValueSets(target hotTarget) map[uint64][]string {
	selected := make(map[uint64][]string)
	for _, value := range stableTargetValues(target) {
		selected[value.target.regionID] = append(selected[value.target.regionID], value.value)
	}
	return selected
}

// remapHotTarget preserves the selected config values across a routing change.
// A repartition may distribute one former target's values over several new
// regions; representing those as members keeps their per-value producers and
// pacing independent after the active phase rebuilds its background lanes.
func remapHotTarget(bundle *regionSnapshotBundle, target hotTarget) (hotTarget, bool) {
	if bundle == nil {
		return hotTarget{}, false
	}
	wanted := make(map[string][]string)
	for _, value := range stableTargetValues(target) {
		wanted[value.target.labelName] = append(wanted[value.target.labelName], value.value)
	}
	if len(wanted) == 0 {
		return hotTarget{}, false
	}
	byRegion := make(map[uint64]*hotTarget)
	for _, current := range bundle.hotTargets {
		for _, value := range wanted[current.labelName] {
			if !containsHotTargetValue(current.values(), value) {
				continue
			}
			member := byRegion[current.regionID]
			if member == nil {
				copy := current
				copy.caseValues = nil
				member = &copy
				byRegion[current.regionID] = member
			}
			if !containsHotTargetValue(member.caseValues, value) {
				member.caseValues = append(member.caseValues, value)
			}
		}
	}
	if len(byRegion) == 0 {
		return hotTarget{}, false
	}
	members := make([]hotTarget, 0, len(byRegion))
	for _, member := range byRegion {
		sort.Strings(member.caseValues)
		member.labelValue = member.caseValues[0]
		members = append(members, *member)
	}
	sort.Slice(members, func(i, j int) bool { return members[i].regionID < members[j].regionID })
	remapped := members[0]
	if len(members) > 1 {
		remapped.members = members
	}
	return remapped, true
}

// chooseStableTargetValue chooses a reproducible configured value for one
// case. Across long-running cases a seeded RNG covers the configured key
// range, while one case retains a stable region-level byte mix.
func chooseStableTargetValue(target hotTarget, random *mathrand.Rand) string {
	values := target.values()
	if len(values) == 0 {
		return ""
	}
	return values[random.Intn(len(values))]
}

func chooseRepartitionTargetValues(target hotTarget, random *mathrand.Rand) []string {
	values := target.values()
	if len(values) < 2 {
		return append([]string(nil), values...)
	}
	// Three evenly spread, config-derived values match the default maximum
	// split parts while keeping every producer's series shape fixed. The seeded
	// starting offset covers different legal split boundaries across long runs.
	count := min(3, len(values))
	start := random.Intn(len(values))
	selected := make([]string, 0, count)
	for index := 0; index < count; index++ {
		value := values[(start+index*len(values)/count)%len(values)]
		if !containsHotTargetValue(selected, value) {
			selected = append(selected, value)
		}
	}
	sort.Strings(selected)
	return selected
}

type regionSnapshotStore struct {
	mu             sync.RWMutex
	current        *regionSnapshotBundle
	mappingChanged chan struct{}
}

func (s *regionSnapshotStore) set(bundle *regionSnapshotBundle) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mappingChanged == nil {
		s.mappingChanged = make(chan struct{})
	}
	if s.current != nil && s.current.mappingVersion != bundle.mappingVersion {
		close(s.mappingChanged)
		s.mappingChanged = make(chan struct{})
	}
	s.current = bundle
}

func (s *regionSnapshotStore) get() *regionSnapshotBundle {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.current
}

// mappingChangeSignal returns a channel that closes only when a later refresh
// discovers a new partition or leader mapping. Snapshot timestamps and ordinary
// statistics refreshes deliberately do not wake focused traffic producers.
func (s *regionSnapshotStore) mappingChangeSignal(version uint64) <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mappingChanged == nil {
		s.mappingChanged = make(chan struct{})
	}
	if s.current != nil && s.current.mappingVersion != version {
		changed := make(chan struct{})
		close(changed)
		return changed
	}
	return s.mappingChanged
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
	byRegion := make(map[string]*hotTarget)
	for _, table := range tables {
		for _, value := range table.LabelValues[labelName] {
			item, _, ok := sharedpartition.FindMetadataByColumnValue(definition, metadata, labelName, value)
			if !ok || item.RegionID == 0 {
				continue
			}
			key := fmt.Sprintf("%d/%s", item.RegionID, labelName)
			target := byRegion[key]
			if target == nil {
				target = &hotTarget{regionID: item.RegionID, labelName: labelName}
				byRegion[key] = target
			}
			if !containsHotTargetValue(target.labelValues, value) {
				target.labelValues = append(target.labelValues, value)
			}
		}
	}
	targets := make([]hotTarget, 0, len(byRegion))
	for _, target := range byRegion {
		sort.Strings(target.labelValues)
		if len(target.labelValues) != 0 {
			target.labelValue = target.labelValues[0]
		}
		targets = append(targets, *target)
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].regionID < targets[j].regionID })
	return targets
}

func selectHotTarget(bundle *regionSnapshotBundle, random *mathrand.Rand, autopilotCase string) (hotTarget, bool) {
	if bundle == nil || len(bundle.hotTargets) == 0 {
		return hotTarget{}, false
	}
	candidates := bundle.hotTargets
	if autopilotCase == autopilotCaseRepartition {
		candidates = make([]hotTarget, 0, len(bundle.hotTargets))
		for _, target := range bundle.hotTargets {
			if len(target.values()) >= 2 {
				candidates = append(candidates, target)
			}
		}
	}
	if len(candidates) == 0 {
		return hotTarget{}, false
	}
	if autopilotCase == autopilotCaseMigration {
		groups := migrationTargetGroups(bundle)
		if len(groups) == 0 {
			return hotTarget{}, false
		}
		group := groups[random.Intn(len(groups))]
		memberCount := requiredMigrationTargetCount(bundle)
		selectedMembers := balancedMigrationTargets(group, bundle.distribution, memberCount)
		for index := range selectedMembers {
			selectedMembers[index].labelValue = chooseStableTargetValue(selectedMembers[index], random)
		}
		selected := selectedMembers[0]
		selected.members = selectedMembers
		return selected, true
	}
	selected := candidates[random.Intn(len(candidates))]
	if autopilotCase == autopilotCaseRepartition {
		selected.caseValues = chooseRepartitionTargetValues(selected, random)
		if len(selected.caseValues) != 0 {
			selected.labelValue = selected.caseValues[0]
		}
	} else {
		selected.labelValue = chooseStableTargetValue(selected, random)
	}
	return selected, true
}

// migrationTargetReady requires both a region surplus and enough hot regions
// led by one datanode. A raw region count alone does not produce a movable
// whole-region candidate: one average-sized region must fit a low target.
func migrationTargetReady(bundle *regionSnapshotBundle) bool {
	return rebalanceTopology(bundle).ready && len(migrationTargetGroups(bundle)) != 0
}

func migrationTargetGroups(bundle *regionSnapshotBundle) [][]hotTarget {
	if bundle == nil {
		return nil
	}
	leaders := make(map[uint64]string, len(bundle.snapshots))
	for _, snapshot := range bundle.snapshots {
		if snapshot.regionID != 0 && snapshot.leader != "" {
			leaders[snapshot.regionID] = snapshot.leader
		}
	}
	byLeader := make(map[string][]hotTarget)
	for _, target := range bundle.hotTargets {
		if leader := leaders[target.regionID]; leader != "" {
			byLeader[leader] = append(byLeader[leader], target)
		}
	}
	memberCount := requiredMigrationTargetCount(bundle)
	groups := make([][]hotTarget, 0, len(byLeader))
	for _, group := range byLeader {
		if len(group) < memberCount {
			continue
		}
		sort.Slice(group, func(i, j int) bool { return group[i].regionID < group[j].regionID })
		groups = append(groups, group)
	}
	return groups
}

// requiredMigrationTargetCount matches the region balancer's target-average-gap
// admission rule. On N datanodes, spreading equal pressure over N regions on
// one source makes each region no larger than the cluster average, so one can
// move to a low target. Requiring N+1 here was needlessly strict: natural
// auto-repartition spreads new regions across nodes and can produce a valid
// N-region source group without ever producing N+1 colocated regions.
func requiredMigrationTargetCount(bundle *regionSnapshotBundle) int {
	if bundle == nil {
		return 2
	}
	count := bundle.datanodeCount
	if count == 0 {
		count = rebalanceTopology(bundle).datanodeCount
	}
	return max(int(count), 2)
}

// balancedMigrationTargets chooses targetCount regions already led by the
// same datanode whose configured series counts occupy the narrowest range.
// The balancer moves a whole region and enforces that the region fits the
// target's gap to the cluster average, so the pressure must be spread over at
// least the datanode count rather than concentrated in a pair.
func balancedMigrationTargets(group []hotTarget, distribution []sharedpartition.RegionDistribution, targetCount int) []hotTarget {
	if targetCount < 2 {
		targetCount = 2
	}
	if len(group) <= targetCount {
		return append([]hotTarget(nil), group...)
	}
	series := make(map[uint64]uint64, len(distribution))
	for _, item := range distribution {
		if item.RegionID != 0 && !item.Unresolved && item.SeriesCount != 0 {
			series[item.RegionID] = item.SeriesCount
		}
	}
	type weightedTarget struct {
		target hotTarget
		series uint64
	}
	known := make([]weightedTarget, 0, len(group))
	for _, target := range group {
		if targetSeries := series[target.regionID]; targetSeries != 0 {
			known = append(known, weightedTarget{target: target, series: targetSeries})
		}
	}
	if len(known) < targetCount {
		return append([]hotTarget(nil), group[:targetCount]...)
	}
	sort.Slice(known, func(i, j int) bool {
		if known[i].series == known[j].series {
			return known[i].target.regionID < known[j].target.regionID
		}
		return known[i].series < known[j].series
	})
	bestStart := 0
	bestRatio := math.Inf(1)
	for start := 0; start+targetCount <= len(known); start++ {
		ratio := math.Log(float64(known[start+targetCount-1].series) / float64(known[start].series))
		if ratio < bestRatio {
			bestStart, bestRatio = start, ratio
		}
	}
	selected := make([]hotTarget, 0, targetCount)
	for _, target := range known[bestStart : bestStart+targetCount] {
		selected = append(selected, target.target)
	}
	sort.Slice(selected, func(i, j int) bool { return selected[i].regionID < selected[j].regionID })
	return selected
}

type caseBackground struct {
	fileConfig samples.FileConfig
	target     hotTarget
}

func describeCaseBackgrounds(backgrounds []caseBackground) string {
	if len(backgrounds) == 0 {
		return "none"
	}
	routes := make([]string, 0, len(backgrounds))
	for _, background := range backgrounds {
		values := background.target.values()
		value := ""
		if len(values) > 0 {
			value = values[0]
		}
		routes = append(routes, fmt.Sprintf("region=%d %s=%s config=%s series=%d", background.target.regionID, background.target.labelName, value, background.fileConfig.Name, background.fileConfig.SeriesCount))
	}
	return strings.Join(routes, "; ")
}

// caseBackgroundPlan chooses the smallest existing config with enough samples
// to keep a region above the scheduler's movable-load floor, while still
// producing one representative value for every current partition-key region.
// This avoids both a full-table scan and a stream of tiny remote-write calls
// that cannot establish destination datanode history.
func caseBackgroundPlan(fileConfigs []samples.FileConfig, bundle *regionSnapshotBundle) []caseBackground {
	if bundle == nil || len(bundle.hotTargets) == 0 {
		return nil
	}
	targets := make([]hotTarget, 0, len(bundle.hotTargets))
	for _, target := range bundle.hotTargets {
		if len(target.values()) == 0 {
			continue
		}
		targets = append(targets, target)
	}
	if len(targets) == 0 {
		return nil
	}
	backgrounds := make([]caseBackground, 0, len(targets))
	for _, target := range targets {
		values := target.values()
		selected := schedulerCaseFileConfig(fileConfigs, target, values[0])
		if selected != nil {
			// FieldGenerators is a mutable per-series cache. The hotspot may use
			// the selected config concurrently, so background generation must not
			// share or even read that map (or a concurrent read/write panic results).
			backgrounds = append(backgrounds, caseBackground{fileConfig: backgroundFileConfig(selected), target: target})
		}
	}
	return backgrounds
}

// backgroundFileConfig copies only immutable generation inputs. FileConfig's
// FieldGenerators is an unsynchronized mutable cache populated by hotspot
// generation, so a struct copy would race merely while reading that map field.
func backgroundFileConfig(source *samples.FileConfig) samples.FileConfig {
	return samples.FileConfig{
		Name:               source.Name,
		Config:             source.Config,
		ReplicaInsertIndex: source.ReplicaInsertIndex,
		SeriesCount:        source.SeriesCount,
		ChurnIndices:       append([]int(nil), source.ChurnIndices...),
	}
}

func schedulerCaseFileConfigs(source []samples.FileConfig, target hotTarget, value string) []samples.FileConfig {
	selected := schedulerCaseFileConfig(source, target, value)
	if selected == nil {
		return nil
	}
	return []samples.FileConfig{backgroundFileConfig(selected)}
}

// schedulerCaseFileConfig returns one deterministic representative config for
// a region. A complete sample-loader pass contains vastly different series
// shapes; even a byte-paced stream therefore yields alternating memtable
// deltas. Scheduler cases deliberately use one compatible shape per region so
// MetaSrv sees a plateau in its own (uncompressed) write-throughput history.
func schedulerCaseFileConfig(source []samples.FileConfig, target hotTarget, value string) *samples.FileConfig {
	if value == "" {
		return nil
	}
	wanted := map[string]struct{}{value: {}}
	var selected *samples.FileConfig
	var fallback *samples.FileConfig
	for index := range source {
		candidate := &source[index]
		if !configSupportsLabelValues(candidate, target.labelName, wanted) {
			continue
		}
		if fallback == nil || candidate.SeriesCount < fallback.SeriesCount {
			fallback = candidate
		}
		if candidate.SeriesCount >= stableBackgroundMinSeries && (selected == nil || candidate.SeriesCount < selected.SeriesCount) {
			selected = candidate
		}
	}
	if selected != nil {
		return selected
	}
	return fallback
}

func configSupportsLabelValues(fileConfig *samples.FileConfig, labelName string, wanted map[string]struct{}) bool {
	for _, tag := range fileConfig.Config.Tags {
		if tag.Name != labelName {
			continue
		}
		available := make(map[string]struct{})
		for _, value := range tag.Dist.LabelGenerator().All() {
			available[value] = struct{}{}
		}
		for value := range wanted {
			if _, ok := available[value]; !ok {
				return false
			}
		}
		return true
	}
	return false
}

func hasHotTarget(bundle *regionSnapshotBundle, target hotTarget) bool {
	if bundle == nil {
		return false
	}
	for _, expected := range target.workloadTargets() {
		found := false
		for _, current := range bundle.hotTargets {
			if current.regionID == expected.regionID && current.labelName == expected.labelName && containsHotTargetValue(current.values(), expected.labelValue) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func matchesHotTarget(series prompb.TimeSeries, target hotTarget) bool {
	for _, label := range series.Labels {
		if label.Name == target.labelName && containsHotTargetValue(target.values(), label.Value) {
			return true
		}
	}
	return false
}

func containsHotTargetValue(values []string, target string) bool {
	for _, value := range values {
		if value == target {
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
	rateHistory   map[uint64][]regionRateSample
	schedule      autopilotSchedule
	rateMode      string
	version       uint64
	store         *regionSnapshotStore
}

func (r *regionRefresher) refresh(ctx context.Context) (*regionSnapshotBundle, []benchmarkEvent, error) {
	now := time.Now()
	snapshots, err := r.client.regionSnapshots(ctx, r.physicalTable, now)
	if err != nil {
		return nil, nil, err
	}
	datanodeCount, err := r.client.activeDatanodeCount(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("query active datanodes from information_schema.cluster_info: %w", err)
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
	hotTargets := configHotTargets(definition, metadata, r.configTables)
	fingerprint := regionMappingFingerprint(snapshots, hotTargets)
	previous := r.store.get()
	if previous == nil || previous.mappingFingerprint != fingerprint {
		r.version++
	}
	bundle := &regionSnapshotBundle{id: newSnapshotID(), timestamp: now, mappingVersion: r.version, mappingFingerprint: fingerprint, snapshots: snapshots, distribution: distribution, logicalTables: r.logicalTables, hotTargets: hotTargets, datanodeCount: datanodeCount}
	for _, snapshot := range snapshots {
		prior, exists := r.previous[snapshot.regionID]
		bundle.events = append(bundle.events, regionEvent(r.base, snapshot, exists, prior))
		bundle.events = append(bundle.events, r.regionStabilityEvent(snapshot, prior, exists))
	}
	changes := mappingChanges(r.base, bundle, r.previous)
	r.previous = make(map[uint64]regionSnapshot, len(snapshots))
	for _, snapshot := range snapshots {
		r.previous[snapshot.regionID] = snapshot
	}
	r.store.set(bundle)
	return bundle, changes, nil
}

// regionMappingFingerprint contains the routing facts that affect a focused
// workload: current region identities/leaders and which configured partition
// values resolve to each region. It intentionally excludes statistics and
// timestamps, which change on every observation but must not restart writers.
func regionMappingFingerprint(snapshots []regionSnapshot, targets []hotTarget) string {
	var builder strings.Builder
	for _, snapshot := range snapshots {
		fmt.Fprintf(&builder, "r:%d:%s:%s:%s\n", snapshot.regionID, snapshot.leader, snapshot.partitionExpression, snapshot.partitionDescription)
	}
	for _, target := range targets {
		fmt.Fprintf(&builder, "t:%d:%s:%s\n", target.regionID, target.labelName, strings.Join(target.values(), ","))
	}
	digest := sha256.Sum256([]byte(builder.String()))
	return hex.EncodeToString(digest[:])
}

// regionStabilityEvent persists a benchmark-side estimate of the raw region
// rate history. It deliberately keeps this separate from the MetaSrv verdict:
// Enterprise currently derives scheduler write load from memtable growth, while
// this estimate uses the monotonic written-bytes counter exposed by the SQL
// statistics view. Recording both lets later analysis distinguish client-side
// traffic pulses from memtable flush artifacts without changing the event schema.
func (r *regionRefresher) regionStabilityEvent(snapshot regionSnapshot, previous regionSnapshot, exists bool) benchmarkEvent {
	event := regionEvent(r.base, snapshot, exists, previous)
	event.EventType = "region_stability_window"

	resetReason := ""
	if !exists {
		resetReason = "region_created"
	} else if previous.leader != snapshot.leader {
		resetReason = "leader_changed"
	} else if event.CounterReset {
		resetReason = "written_bytes_counter_reset"
	}
	if resetReason != "" {
		delete(r.rateHistory, snapshot.regionID)
	}
	if exists && resetReason == "" && event.IntervalMS > 0 {
		r.rateHistory[snapshot.regionID] = append(r.rateHistory[snapshot.regionID], regionRateSample{
			timestamp: snapshot.timestamp,
			writeBPS:  float64(event.WrittenBytesBPS),
		})
	}

	maxSamples := max(r.schedule.maxHistoryWindows*r.schedule.repartitionSamples, r.schedule.repartitionSamples)
	history := r.rateHistory[snapshot.regionID]
	if len(history) > maxSamples {
		history = append([]regionRateSample(nil), history[len(history)-maxSamples:]...)
		r.rateHistory[snapshot.regionID] = history
	}
	rates := make([]float64, 0, len(history))
	for _, sample := range history {
		rates = append(rates, sample.writeBPS)
	}
	mean, cv := rateMeanAndCV(rates)
	details, _ := json.Marshal(map[string]any{
		"source":                  "benchmark_region_statistics",
		"autopilot_rate_mode":     r.rateMode,
		"throughput_source":       "written_bytes_since_open_delta",
		"scheduler_write_metric":  "metasrv_memtable_size_delta_ewma",
		"history_samples_bps":     rates,
		"sample_count":            len(rates),
		"required_samples":        r.schedule.repartitionSamples,
		"max_region_history_cv":   r.schedule.maxRegionHistoryCV,
		"mean_write_bps":          mean,
		"cv":                      cv,
		"stable_estimate":         len(rates) >= r.schedule.repartitionSamples && cv <= r.schedule.maxRegionHistoryCV,
		"history_reset_reason":    resetReason,
		"memtable_flush_observed": event.MemtableDecreased,
		"history_capacity":        maxSamples,
		"history_window_count":    r.schedule.maxHistoryWindows,
	})
	event.Details = string(details)
	return event
}

func rateMeanAndCV(rates []float64) (float64, float64) {
	if len(rates) == 0 {
		return 0, 0
	}
	mean := 0.0
	for _, rate := range rates {
		mean += rate
	}
	mean /= float64(len(rates))
	if mean <= 0 {
		return mean, 0
	}
	variance := 0.0
	for _, rate := range rates {
		delta := rate - mean
		variance += delta * delta
	}
	return mean, math.Sqrt(variance/float64(len(rates))) / mean
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

func (c httpSQLClient) activeDatanodeCount(ctx context.Context) (uint64, error) {
	rows, err := c.query(ctx, `SELECT peer_id FROM information_schema.cluster_info WHERE peer_type = 'DATANODE' AND active_time IS NOT NULL`)
	if err != nil {
		return 0, err
	}
	seen := make(map[uint64]struct{}, len(rows))
	for _, row := range rows {
		seen[uintValue(row["peer_id"])] = struct{}{}
	}
	return uint64(len(seen)), nil
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
