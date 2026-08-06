package sampleloader

import (
	"context"
	"encoding/json"
	"io"
	"math"
	mathrand "math/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sharedpartition "metrics-bench-suite/pkg/partition"
	"metrics-bench-suite/pkg/samples"

	"github.com/prometheus/prometheus/prompb"
)

func TestPeriodicOptionsRequireExplicitExpectation(t *testing.T) {
	cmd := NewCommand()
	for name, value := range map[string]string{
		"target-physical-table":       "metrics_physical",
		"pressure-high-min-write-bps": "1",
		"baseline-duration":           "0s",
		"burst-active-duration":       "1s",
		"transient-burst-duration":    "1s",
		"burst-period":                "1s",
		"observe-interval":            "1s",
	} {
		if err := cmd.Flags().Set(name, value); err != nil {
			t.Fatal(err)
		}
	}
	loader := &SampleLoader{DryRun: true, TickInterval: time.Second, Workers: 1}
	if _, err := periodicOptionsFromCommand(cmd, loader); err == nil || !strings.Contains(err.Error(), "autopilot-expect") {
		t.Fatalf("expected explicit autopilot expectation error, got %v", err)
	}

	if err := cmd.Flags().Set("autopilot-expect", "repartition"); err != nil {
		t.Fatal(err)
	}
	options, err := periodicOptionsFromCommand(cmd, loader)
	if err != nil {
		t.Fatal(err)
	}
	if options.TargetPhysicalTable != "metrics_physical" {
		t.Fatalf("unexpected target physical table: %q", options.TargetPhysicalTable)
	}
}

func TestPeriodicCaseSuppliesItsOwnExpectation(t *testing.T) {
	cmd := NewCommand()
	for name, value := range map[string]string{
		"target-physical-table":       "metrics_physical",
		"pressure-high-min-write-bps": "1",
		"baseline-duration":           "0s",
		"burst-active-duration":       "1s",
		"transient-burst-duration":    "1s",
		"burst-period":                "1s",
		"observe-interval":            "1s",
		"autopilot-case":              "migration",
	} {
		if err := cmd.Flags().Set(name, value); err != nil {
			t.Fatal(err)
		}
	}
	options, err := periodicOptionsFromCommand(cmd, &SampleLoader{DryRun: true, TickInterval: time.Second, Workers: 1})
	if err != nil {
		t.Fatal(err)
	}
	if options.BurstClass != burstClassQualified || options.expectedAutopilotAction(burstClassQualified) != "rebalance" {
		t.Fatalf("migration case must use a qualified rebalance plan: %#v", options)
	}
}

func TestSteadyTrafficDefaultsToMixedBursts(t *testing.T) {
	cmd := NewCommand()
	for name, value := range map[string]string{
		"target-physical-table":       "metrics_physical",
		"pressure-high-min-write-bps": "8",
		"baseline-duration":           "0s",
		"burst-active-duration":       "1s",
		"transient-burst-duration":    "1s",
		"burst-period":                "1s",
		"observe-interval":            "1s",
		"autopilot-expect":            "both",
		"periodic-traffic-mode":       "steady",
	} {
		if err := cmd.Flags().Set(name, value); err != nil {
			t.Fatal(err)
		}
	}
	options, err := periodicOptionsFromCommand(cmd, &SampleLoader{DryRun: true, TickInterval: time.Second, Workers: 1})
	if err != nil {
		t.Fatal(err)
	}
	if options.BurstClass != burstClassMixed || options.BaselineTargetWriteBPS != 2 || options.QualifiedMaxWriteBPS != 24 || options.BurstGap != defaultBurstGap {
		t.Fatalf("unexpected steady defaults: %#v", options)
	}
}

func TestContinuousProfileRemainsTheDefault(t *testing.T) {
	cmd := NewCommand()
	profile, err := cmd.Flags().GetString("load-profile")
	if err != nil {
		t.Fatal(err)
	}
	if profile != "continuous" {
		t.Fatalf("expected continuous default profile, got %q", profile)
	}
}

func TestReplaceEndpointPath(t *testing.T) {
	got, err := replaceEndpointPath("https://example.com/v1/prometheus/write?db=public", "/v1/sql")
	if err != nil {
		t.Fatal(err)
	}
	if want := "https://example.com/v1/sql"; got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

func TestRedactConfigSnapshot(t *testing.T) {
	snapshot := "[auto_repartition]\nmin_samples = 3\npassword = 'secret'\napi_token: abc"
	got := redactConfigSnapshot(snapshot)
	if strings.Contains(got, "secret") || strings.Contains(got, "abc") || !strings.Contains(got, "min_samples = 3") {
		t.Fatalf("unexpected redacted snapshot: %q", got)
	}
}

func TestDecodeSQLRows(t *testing.T) {
	rows, err := decodeSQLRows([]byte(`{"output":[{"records":{"schema":{"column_schemas":[{"name":"region_id"},{"name":"peer_id"}]},"rows":[[42,"node-a"]]}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || uintValue(rows[0]["region_id"]) != 42 || stringValue(rows[0]["peer_id"]) != "node-a" {
		t.Fatalf("unexpected rows: %#v", rows)
	}
}

func TestEventWriterIngestsNDJSON(t *testing.T) {
	var received benchmarkEvent
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/ingest" {
			t.Errorf("unexpected path: %s", request.URL.Path)
		}
		if request.URL.Query().Get("db") != "public" || request.URL.Query().Get("table") != "benchmark_autopilot_events" {
			t.Errorf("unexpected query: %s", request.URL.RawQuery)
		}
		if request.Header.Get("Content-Type") != "application/x-ndjson" {
			t.Errorf("unexpected content type: %q", request.Header.Get("Content-Type"))
		}
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(body, &received); err != nil {
			t.Fatal(err)
		}
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	writer := newEventWriter(periodicOptions{MonitoringURL: server.URL + "/v1/ingest"})
	writer.emit(context.Background(), benchmarkEvent{RunID: "run-1", EventType: "run_started"})
	if err := writer.close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if received.RunID != "run-1" || received.EventSequence != 1 || received.EventTSMS == 0 {
		t.Fatalf("unexpected event: %#v", received)
	}
}

func TestBenchmarkEventEncodesZeroFloatFieldsAsFloat64(t *testing.T) {
	encoded, err := json.Marshal(benchmarkEvent{RunID: "run-1", EventType: "pressure_started"})
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"\"write_bps\":0.0", "\"latency_max_ms\":0.0", "\"pressure_threshold_bps\":0.0", "\"written_bytes_bps\":0.0"} {
		if !strings.Contains(string(encoded), field) {
			t.Fatalf("expected %s in encoded event: %s", field, encoded)
		}
	}
}

func TestEventWriterRecoversAfterMonitoringFailure(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		attempts++
		if attempts <= 3 {
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	writer := newEventWriter(periodicOptions{MonitoringURL: server.URL + "/v1/ingest"})
	if err := writer.emitAndFlush(context.Background(), benchmarkEvent{RunID: "run-1", EventType: "pressure_started"}); err == nil {
		t.Fatal("expected initial monitoring failure")
	}
	if err := writer.emitAndFlush(context.Background(), benchmarkEvent{RunID: "run-1", EventType: "workload_window"}); err != nil {
		t.Fatalf("expected recovery after monitoring failure, got %v", err)
	}
}

func TestEventWriterFlushesLifecycleEventImmediately(t *testing.T) {
	received := make(chan benchmarkEvent, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var event benchmarkEvent
		if err := json.NewDecoder(request.Body).Decode(&event); err != nil {
			t.Fatal(err)
		}
		received <- event
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	writer := newEventWriter(periodicOptions{MonitoringURL: server.URL + "/v1/ingest"})
	if err := writer.emitAndFlush(context.Background(), benchmarkEvent{RunID: "run-1", EventType: "pressure_scheduled"}); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-received:
		if event.EventType != "pressure_scheduled" || event.EventSequence != 1 {
			t.Fatalf("unexpected event: %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("lifecycle event was not flushed")
	}
}

func TestWorkloadWindowFlushesCurrentSnapshotWithoutReplayingRegionEvents(t *testing.T) {
	var received []benchmarkEvent
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		decoder := json.NewDecoder(request.Body)
		for {
			var event benchmarkEvent
			if err := decoder.Decode(&event); err != nil {
				if err == io.EOF {
					break
				}
				t.Fatal(err)
			}
			received = append(received, event)
		}
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	writer := newEventWriter(periodicOptions{MonitoringURL: server.URL + "/v1/ingest"})
	snapshots := &regionSnapshotStore{}
	snapshots.set(&regionSnapshotBundle{
		id:        "snapshot-1",
		timestamp: time.Unix(100, 0),
		events:    []benchmarkEvent{{EventType: "region_stats_snapshot"}},
	})
	if err := emitWithCurrentSnapshotAndFlush(writer, context.Background(), benchmarkEvent{RunID: "run-1", EventType: "workload_window"}, snapshots); err != nil {
		t.Fatal(err)
	}
	if len(received) != 1 || received[0].EventType != "workload_window" || !strings.Contains(received[0].Details, `"snapshot_id":"snapshot-1"`) {
		t.Fatalf("unexpected immediately flushed workload evidence: %#v", received)
	}
}

func TestSeededBurstScheduleIsDeterministic(t *testing.T) {
	first, firstSeed := newPeriodicRandom(42)
	second, secondSeed := newPeriodicRandom(42)
	if firstSeed != 42 || secondSeed != 42 {
		t.Fatalf("unexpected seeds: %d, %d", firstSeed, secondSeed)
	}
	if got, want := jitterDuration(time.Hour, 0.2, first), jitterDuration(time.Hour, 0.2, second); got != want {
		t.Fatalf("seeded jitter differs: %s != %s", got, want)
	}
}

func TestMixedBurstClassesAreSeededAndContainBothCategories(t *testing.T) {
	first, _ := newPeriodicRandom(42)
	second, _ := newPeriodicRandom(42)
	firstScheduler := newBurstClassScheduler(burstClassMixed, first)
	secondScheduler := newBurstClassScheduler(burstClassMixed, second)
	for range 8 {
		firstClass := firstScheduler.next()
		if firstClass != secondScheduler.next() {
			t.Fatal("mixed schedule must be deterministic with a seed")
		}
		secondClass := firstScheduler.next()
		if secondClass != secondScheduler.next() {
			t.Fatal("mixed schedule must be deterministic with a seed")
		}
		if firstClass == secondClass {
			t.Fatalf("mixed scheduler must pair transient and qualified bursts, got %q then %q", firstClass, secondClass)
		}
	}
}

func TestBurstClassDefinesExpectationAndTarget(t *testing.T) {
	options := periodicOptions{TransientBurstDuration: 5 * time.Minute, BurstActiveDuration: 30 * time.Minute, BaselineTargetWriteBPS: 2, PressureHighMinWriteBPS: 8}
	if got := burstClassTransient.expectation("both"); got != "none" {
		t.Fatalf("transient expectation = %q", got)
	}
	if got := burstClassQualified.initialTargetBPS(options); got != 12 {
		t.Fatalf("qualified target = %v", got)
	}
	if got := burstClassTransient.duration(options); got != 5*time.Minute {
		t.Fatalf("transient duration = %s", got)
	}
}

func TestSteadyBurstGapStartsAfterActivePhase(t *testing.T) {
	previousStart := time.Unix(100, 0)
	activeFinished := time.Unix(200, 0)
	options := periodicOptions{TrafficMode: "steady", BurstGap: 15 * time.Minute, BurstPeriod: time.Hour, BaselineTickInterval: time.Minute}
	if got, want := nextBurstStart(options, previousStart, activeFinished, mathrand.New(mathrand.NewSource(1))), activeFinished.Add(15*time.Minute); !got.Equal(want) {
		t.Fatalf("steady next burst = %s, want %s", got, want)
	}
}

func TestLegacyBurstPeriodStillUsesStartToStartScheduling(t *testing.T) {
	previousStart := time.Unix(100, 0)
	activeFinished := time.Unix(150, 0)
	options := periodicOptions{TrafficMode: "legacy", BurstPeriod: time.Hour, BurstJitter: 0, BaselineTickInterval: time.Minute}
	if got, want := nextBurstStart(options, previousStart, activeFinished, mathrand.New(mathrand.NewSource(1))), previousStart.Add(time.Hour); !got.Equal(want) {
		t.Fatalf("legacy next burst = %s, want %s", got, want)
	}
}

func TestPlannedBurstEventRecordsSeedAndSchedule(t *testing.T) {
	scheduled := time.Unix(100, 0)
	event := plannedBurstEvent(benchmarkEvent{}, 2, scheduled, periodicOptions{BurstActiveDuration: time.Minute, TransientBurstDuration: time.Second, BurstPeriod: time.Hour, BurstJitter: 0.2, PressureHighMinWriteBPS: 1024, BaselineTargetWriteBPS: 256, AutopilotSchedule: defaultAutopilotSchedule()}, 42, "repartition", burstPlan{class: burstClassQualified, duration: time.Minute, targetWriteBPS: 1536, maximumWriteBPS: 3072, pressureScale: 1.0}, hotTarget{regionID: 7, labelName: "namespace", labelValue: "app-1"}, true, &regionSnapshotBundle{snapshots: []regionSnapshot{{regionID: 1, leader: "node-a"}, {regionID: 2, leader: "node-b"}, {regionID: 3, leader: "node-a"}}})
	if event.EventType != "pressure_scheduled" || event.Phase != "baseline" || event.Cycle != 2 || event.ScheduledTSMS != scheduled.UnixMilli() {
		t.Fatalf("unexpected planned event: %#v", event)
	}
	if !strings.Contains(event.Details, `"random_seed":42`) || !strings.Contains(event.Details, `"burst_jitter":0.2`) || !strings.Contains(event.Details, `"rebalance_topology_ready":true`) {
		t.Fatalf("planned event missing scheduling details: %s", event.Details)
	}
}

func TestRebalanceTopologyRequiresMoreRegionsThanObservedDatanodes(t *testing.T) {
	topology := rebalanceTopology(&regionSnapshotBundle{snapshots: []regionSnapshot{{regionID: 1, leader: "node-a"}, {regionID: 2, leader: "node-b"}, {regionID: 3, leader: "node-a"}}})
	if topology.regionCount != 3 || topology.datanodeCount != 2 || topology.regionSurplus != 1 || !topology.ready {
		t.Fatalf("unexpected ready topology: %#v", topology)
	}
	topology = rebalanceTopology(&regionSnapshotBundle{snapshots: []regionSnapshot{{regionID: 1, leader: "node-a"}, {regionID: 2, leader: "node-b"}}})
	if topology.ready {
		t.Fatalf("topology without region surplus must not be ready: %#v", topology)
	}
}

func TestWorkloadCollectorSeparatesBaselineAndHotspotTraffic(t *testing.T) {
	collector := &workloadCollector{}
	collector.add(writeResult{lane: baselineTrafficLane, requestCount: 1, sampleCount: 2, payloadBytes: 100})
	collector.add(writeResult{lane: hotspotTrafficLane, requestCount: 3, sampleCount: 4, payloadBytes: 200})
	metrics := collector.reset()
	if metrics.payloadBytes != 300 || metrics.baseline.payloadBytes != 100 || metrics.hotspot.payloadBytes != 200 || metrics.baseline.sampleCount != 2 || metrics.hotspot.requestCount != 3 {
		t.Fatalf("unexpected lane metrics: %#v", metrics)
	}
}

func TestEnqueuePeriodicRequestsPreservesTrafficLaneAndPacer(t *testing.T) {
	requests := make(chan periodicWriteRequest, 1)
	pacer := newBytePacer(1)
	done := make(chan bool, 1)
	go func() {
		done <- enqueuePeriodicRequests(context.Background(), requests, hotspotTrafficLane, pacer, func(raw chan<- prompb.WriteRequest) bool {
			raw <- prompb.WriteRequest{Timeseries: []prompb.TimeSeries{{Samples: []prompb.Sample{{Timestamp: 1}}}}}
			return true
		})
	}()
	queued := <-requests
	if queued.lane != hotspotTrafficLane || queued.pacer != pacer || sampleCount(queued.request) != 1 {
		t.Fatalf("unexpected queued periodic request: %#v", queued)
	}
	if !<-done {
		t.Fatal("enqueue unexpectedly failed")
	}
}

func TestFocusedCaseProducesBaselineAndHotspotConcurrently(t *testing.T) {
	var requests atomic.Uint64
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = io.Copy(io.Discard, request.Body)
		requests.Add(1)
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	lower, upper := 0.0, 1.0
	fileConfigs := []samples.FileConfig{{
		Name: "representative",
		Config: samples.Config{
			Tags: []samples.Tag{{
				Name: "namespace",
				Dist: samples.Distribution{Type: "weighted_preset", Preset: []samples.PresetItem{
					{Value: "app-0", Weight: 1}, {Value: "app-1", Weight: 1},
				}},
			}},
			Fields: []samples.Field{{Name: "value", Dist: samples.Distribution{Type: "uniform", LowerBound: &lower, UpperBound: &upper}}},
		},
	}}
	snapshots := &regionSnapshotStore{}
	snapshots.set(&regionSnapshotBundle{hotTargets: []hotTarget{
		{regionID: 1, labelName: "namespace", labelValues: []string{"app-0"}},
		{regionID: 2, labelName: "namespace", labelValues: []string{"app-1"}},
	}})
	loader := &SampleLoader{
		RemoteWriteURL: server.URL,
		Interval:       time.Millisecond,
		MaxSamples:     10,
		Workers:        2,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	current := time.Now()
	err := loader.runPeriodicWrites(ctx, fileConfigs, &current, samples.NewChurnEpochGenerator(0), time.Now().Add(time.Second), time.Millisecond, "active", 1, periodicOptions{
		TrafficMode:            "steady",
		AutopilotCase:          "repartition",
		BaselineTargetWriteBPS: 1 << 30,
	}, nil, snapshots, benchmarkEvent{}, &hotTarget{regionID: 2, labelName: "namespace", labelValues: []string{"app-1"}}, burstPlan{class: burstClassQualified, targetWriteBPS: 1 << 30, maximumWriteBPS: 1 << 30})
	if err != context.DeadlineExceeded {
		t.Fatalf("focused case error = %v, want context deadline", err)
	}
	if got := requests.Load(); got < 2 {
		t.Fatalf("expected concurrent baseline and hotspot writes, got %d requests", got)
	}
}

func TestBurstAmplifierAllowsSmallHotspotsToReachPressure(t *testing.T) {
	amplifier := newBurstAmplifier(4)
	for amplifier.value() < 256 {
		if _, increased := amplifier.increase(); !increased {
			t.Fatalf("amplifier stopped at %d before the small-hotspot cap", amplifier.value())
		}
	}
	if amplifier.value() != 256 {
		t.Fatalf("amplifier cap = %d, want 256", amplifier.value())
	}
}

func TestExplicitCaseStartsWithEnoughValueConstrainedPasses(t *testing.T) {
	if got := initialBurstAmplification(4, true); got != minCaseBurstPasses {
		t.Fatalf("explicit case initial passes = %d, want %d", got, minCaseBurstPasses)
	}
	if got := initialBurstAmplification(128, true); got != 128 {
		t.Fatalf("explicit case must preserve larger configured passes, got %d", got)
	}
	if got := initialBurstAmplification(4, false); got != 4 {
		t.Fatalf("legacy path must preserve configured passes, got %d", got)
	}
}

func TestExplicitQualifiedCaseReservesSourceDerivedStabilizationPlateau(t *testing.T) {
	options := periodicOptions{
		BurstActiveDuration: time.Minute,
		AutopilotCase:       "repartition",
		AutopilotSchedule: autopilotSchedule{
			samplingWindow:     45 * time.Second,
			maxHistoryWindows:  5,
			repartitionSamples: 3,
		},
	}
	plan := newBurstPlan(options, burstClassQualified, mathrand.New(mathrand.NewSource(1)))
	if want := 6 * 45 * time.Second; plan.duration != want {
		t.Fatalf("qualified case duration = %s, want %s", plan.duration, want)
	}
	if legacy := newBurstPlan(periodicOptions{BurstActiveDuration: time.Minute}, burstClassQualified, mathrand.New(mathrand.NewSource(1))); legacy.duration != time.Minute {
		t.Fatalf("legacy qualified duration = %s, want configured value", legacy.duration)
	}
}

func TestExplicitCaseRetainsSmallStableBaseline(t *testing.T) {
	options := periodicOptions{BaselineTargetWriteBPS: 2 * 1024 * 1024}
	plan := burstPlan{targetWriteBPS: 12 * 1024 * 1024}
	if got, want := activeBaselineTarget(options, plan, true), min(options.BaselineTargetWriteBPS, plan.targetWriteBPS*caseBaselineRatio); got != want {
		t.Fatalf("focused baseline = %f, want %f", got, want)
	}
	if got := activeBaselineTarget(options, plan, false); got != options.BaselineTargetWriteBPS {
		t.Fatalf("legacy baseline = %f, want %f", got, options.BaselineTargetWriteBPS)
	}
}

func TestExplicitCaseStopsAdaptingAfterPressureFloor(t *testing.T) {
	if got := pressureAdjustmentFloor(100, 200, true); math.Abs(got-110) > 1e-9 {
		t.Fatalf("explicit case adjustment floor = %f, want 110", got)
	}
	if got := pressureAdjustmentFloor(100, 80, true); got != 80 {
		t.Fatalf("explicit case must not exceed its configured target, got %f", got)
	}
	if got := pressureAdjustmentFloor(100, 200, false); got != 200 {
		t.Fatalf("legacy adjustment floor = %f, want configured target", got)
	}
}

func TestTransientPlanIsRandomButBelowSchedulerHistoryAndPressureThreshold(t *testing.T) {
	options := periodicOptions{
		BaselineTickInterval:    15 * time.Second,
		TransientBurstDuration:  5 * time.Minute,
		PressureHighMinWriteBPS: 8 * 1024 * 1024,
		AutopilotSchedule:       defaultAutopilotSchedule(),
	}
	random := mathrand.New(mathrand.NewSource(42))
	plan := newBurstPlan(options, burstClassTransient, random)
	if plan.duration < options.BaselineTickInterval || plan.duration >= 2*options.AutopilotSchedule.samplingWindow {
		t.Fatalf("transient duration %s must remain below the %s history horizon", plan.duration, 2*options.AutopilotSchedule.samplingWindow)
	}
	if plan.targetWriteBPS <= 0 || plan.targetWriteBPS >= options.PressureHighMinWriteBPS {
		t.Fatalf("transient target %f must remain below high threshold %f", plan.targetWriteBPS, options.PressureHighMinWriteBPS)
	}
	if plan.pressureScale < minTransientScale || plan.pressureScale > maxTransientScale {
		t.Fatalf("transient scale %f outside [%f,%f]", plan.pressureScale, minTransientScale, maxTransientScale)
	}
}

func TestMigrationCaseBootstrapsRepartitionUntilTopologyHasRegionSurplus(t *testing.T) {
	withoutSurplus := &regionSnapshotBundle{datanodeCount: 3, snapshots: []regionSnapshot{{regionID: 1}, {regionID: 2}, {regionID: 3}}}
	if got := effectiveAutopilotCase("migration", withoutSurplus); got != "repartition" {
		t.Fatalf("migration case without surplus = %q, want repartition bootstrap", got)
	}
	withSurplus := &regionSnapshotBundle{datanodeCount: 3, snapshots: []regionSnapshot{{regionID: 1}, {regionID: 2}, {regionID: 3}, {regionID: 4}}}
	if got := effectiveAutopilotCase("migration", withSurplus); got != "migration" {
		t.Fatalf("migration case with surplus = %q, want migration", got)
	}
}

func TestConfigHotTargetsUseConfiguredPartitionValues(t *testing.T) {
	definition, err := sharedpartition.ParsePartitionDefinition("PARTITION ON COLUMNS (namespace) (namespace < 'app-1', namespace >= 'app-1')")
	if err != nil {
		t.Fatal(err)
	}
	targets := configHotTargets(definition, []sharedpartition.Metadata{
		{Name: "p0", Ordinal: 1, Expression: "namespace", Description: "namespace < 'app-1'", RegionID: 42},
		{Name: "p1", Ordinal: 2, Expression: "namespace", Description: "namespace >= 'app-1'", RegionID: 43},
	}, []sharedpartition.ConfigTable{{LabelValues: map[string][]string{"namespace": {"app-0", "app-1"}}}})
	if len(targets) != 2 || targets[0].regionID != 42 || targets[1].regionID != 43 {
		t.Fatalf("unexpected hot targets: %#v", targets)
	}
	if !matchesHotTarget(prompb.TimeSeries{Labels: []prompb.Label{{Name: "namespace", Value: "app-1"}}}, targets[1]) {
		t.Fatal("expected matching target label")
	}
}

func TestConfigHotTargetsGroupsMultipleValuesInOneRegion(t *testing.T) {
	definition, err := sharedpartition.ParsePartitionDefinition("PARTITION ON COLUMNS (namespace) (namespace < 'app-3', namespace >= 'app-3')")
	if err != nil {
		t.Fatal(err)
	}
	targets := configHotTargets(definition, []sharedpartition.Metadata{
		{Name: "p0", Ordinal: 1, Expression: "namespace", Description: "namespace < 'app-3'", RegionID: 42},
		{Name: "p1", Ordinal: 2, Expression: "namespace", Description: "namespace >= 'app-3'", RegionID: 43},
	}, []sharedpartition.ConfigTable{{LabelValues: map[string][]string{"namespace": {"app-0", "app-1", "app-2", "app-3"}}}})
	if len(targets) != 2 || len(targets[0].values()) != 3 || targets[0].labelValue != "app-0" {
		t.Fatalf("unexpected grouped hot targets: %#v", targets)
	}
	if _, ok := selectHotTarget(&regionSnapshotBundle{hotTargets: targets}, mathrand.New(mathrand.NewSource(1)), "repartition"); !ok {
		t.Fatal("expected a multi-value repartition target")
	}
}

func TestMigrationTargetUsesColocatedRegions(t *testing.T) {
	bundle := &regionSnapshotBundle{
		hotTargets: []hotTarget{
			{regionID: 1, labelName: "namespace", labelValues: []string{"app-0"}},
			{regionID: 2, labelName: "namespace", labelValues: []string{"app-1"}},
			{regionID: 3, labelName: "namespace", labelValues: []string{"app-2"}},
		},
		snapshots: []regionSnapshot{
			{regionID: 1, leader: "dn-1"},
			{regionID: 2, leader: "dn-1"},
			{regionID: 3, leader: "dn-2"},
		},
	}
	target, ok := selectHotTarget(bundle, mathrand.New(mathrand.NewSource(1)), "migration")
	if !ok {
		t.Fatal("expected a colocated migration target")
	}
	if got := hotTargetRegionIDs(target); !reflect.DeepEqual(got, []uint64{1, 2}) {
		t.Fatalf("migration target regions = %v, want colocated regions [1 2]", got)
	}
}

func TestMigrationTargetRequiresColocation(t *testing.T) {
	bundle := &regionSnapshotBundle{
		hotTargets: []hotTarget{
			{regionID: 1, labelName: "namespace", labelValues: []string{"app-0"}},
			{regionID: 2, labelName: "namespace", labelValues: []string{"app-1"}},
		},
		snapshots: []regionSnapshot{{regionID: 1, leader: "dn-1"}, {regionID: 2, leader: "dn-2"}},
	}
	if _, ok := selectHotTarget(bundle, mathrand.New(mathrand.NewSource(1)), "migration"); ok {
		t.Fatal("migration must not pretend a one-region-per-node topology can rebalance")
	}
}

func TestBalancedMigrationTargetsPreferComparableConfiguredVolume(t *testing.T) {
	group := []hotTarget{{regionID: 1}, {regionID: 2}, {regionID: 3}}
	got := balancedMigrationTargets(group, []sharedpartition.RegionDistribution{
		{RegionID: 1, SeriesCount: 10},
		{RegionID: 2, SeriesCount: 1_000},
		{RegionID: 3, SeriesCount: 900},
	})
	if ids := hotTargetRegionIDs(hotTarget{members: got}); !reflect.DeepEqual(ids, []uint64{2, 3}) {
		t.Fatalf("balanced migration regions = %v, want [2 3]", ids)
	}
}

func TestCaseBackgroundPlanUsesConfigDerivedValueForEachRegion(t *testing.T) {
	fileConfigs := []samples.FileConfig{{
		Name: "representative",
		Config: samples.Config{Tags: []samples.Tag{{
			Name: "namespace",
			Dist: samples.Distribution{Type: "weighted_preset", Preset: []samples.PresetItem{
				{Value: "app-0", Weight: 1},
				{Value: "app-1", Weight: 1},
			}},
		}}},
	}}
	bundle := &regionSnapshotBundle{hotTargets: []hotTarget{
		{regionID: 1, labelName: "namespace", labelValues: []string{"app-0"}},
		{regionID: 2, labelName: "namespace", labelValues: []string{"app-1"}},
	}}
	backgrounds := caseBackgroundPlan(fileConfigs, bundle)
	if len(backgrounds) != 2 || backgrounds[0].fileConfig.Name != "representative" || backgrounds[1].target.regionID != 2 {
		t.Fatalf("unexpected case background plan: %#v", backgrounds)
	}
}

func TestCaseBackgroundPlanPrefersLargerConfigForStableSampling(t *testing.T) {
	fileConfigs := []samples.FileConfig{
		{Name: "small", SeriesCount: 1, FieldGenerators: map[string]samples.FloatGenerator{}, Config: samples.Config{Tags: []samples.Tag{{Name: "namespace", Dist: samples.Distribution{Type: "weighted_preset", Preset: []samples.PresetItem{{Value: "app-0", Weight: 1}}}}}}},
		{Name: "large", SeriesCount: 100, FieldGenerators: map[string]samples.FloatGenerator{}, Config: samples.Config{Tags: []samples.Tag{{Name: "namespace", Dist: samples.Distribution{Type: "weighted_preset", Preset: []samples.PresetItem{{Value: "app-0", Weight: 1}}}}}}},
	}
	backgrounds := caseBackgroundPlan(fileConfigs, &regionSnapshotBundle{hotTargets: []hotTarget{{regionID: 1, labelName: "namespace", labelValues: []string{"app-0"}}}})
	if len(backgrounds) != 1 || backgrounds[0].fileConfig.Name != "large" {
		t.Fatalf("expected largest representative config, got %#v", backgrounds)
	}
	if backgrounds[0].fileConfig.FieldGenerators != nil {
		t.Fatal("background config must not share mutable field-generator cache")
	}
}

func TestEventWriterBindsWorkloadEventToRegionSnapshot(t *testing.T) {
	var received []benchmarkEvent
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		decoder := json.NewDecoder(request.Body)
		for {
			var event benchmarkEvent
			if err := decoder.Decode(&event); err != nil {
				if err == io.EOF {
					break
				}
				t.Fatal(err)
			}
			received = append(received, event)
		}
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	bundle := &regionSnapshotBundle{
		id:             "snapshot-1",
		timestamp:      time.Unix(10, 0),
		mappingVersion: 2,
		events:         []benchmarkEvent{regionEvent(benchmarkEvent{RunID: "run-1"}, regionSnapshot{timestamp: time.Unix(10, 0), regionID: 42, partition: "p0"}, false, regionSnapshot{})},
	}
	writer := newEventWriter(periodicOptions{MonitoringURL: server.URL + "/v1/ingest"})
	writer.emitWithSnapshot(context.Background(), benchmarkEvent{RunID: "run-1", EventType: "workload_window"}, bundle)
	if err := writer.close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(received) != 2 || received[0].EventType != "workload_window" || received[1].EventType != "region_stats_snapshot" {
		t.Fatalf("unexpected snapshot bundle: %#v", received)
	}
	for _, event := range received {
		var details map[string]any
		if err := json.Unmarshal([]byte(event.Details), &details); err != nil || details["snapshot_id"] != "snapshot-1" || details["mapping_version"] != float64(2) {
			t.Fatalf("event is not bound to snapshot: %#v, %v", event, err)
		}
	}
}

func TestHTTPClientUsesFormSQLAndDatabase(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/sql" {
			t.Errorf("unexpected path: %s", request.URL.Path)
		}
		if request.URL.Query().Get("db") != "target" {
			t.Errorf("unexpected db: %s", request.URL.RawQuery)
		}
		body, _ := io.ReadAll(request.Body)
		form, err := url.ParseQuery(string(body))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(form.Get("sql"), "information_schema.partitions") {
			t.Fatalf("unexpected SQL: %s", form.Get("sql"))
		}
		_, _ = writer.Write([]byte(`{"output":[{"records":{"schema":{"column_schemas":[{"name":"partition_name"},{"name":"partition_expression"},{"name":"partition_description"},{"name":"greptime_partition_id"},{"name":"peer_id"},{"name":"table_id"},{"name":"region_number"},{"name":"region_rows"},{"name":"written_bytes_since_open"},{"name":"query_cpu_time_millis"},{"name":"query_scanned_bytes"},{"name":"disk_size"},{"name":"memtable_size"},{"name":"manifest_size"},{"name":"sst_size"},{"name":"sst_num"},{"name":"index_size"},{"name":"engine"},{"name":"region_role"}]},"rows":[["p0","namespace","namespace < 'app-1'",42,"node-a",1,0,10,20,30,40,50,60,70,80,2,90,"mito","Leader"]]}}]}`))
	}))
	defer server.Close()

	client := httpSQLClient{endpoint: server.URL + "/v1/sql", database: "target", client: server.Client()}
	snapshots, err := client.regionSnapshots(context.Background(), "physical", time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshots) != 1 || snapshots[0].regionID != 42 || snapshots[0].memtableSize != 60 || snapshots[0].leader != "node-a" {
		t.Fatalf("unexpected snapshots: %#v", snapshots)
	}
}

func TestRegionEventMarksCounterReset(t *testing.T) {
	previous := regionSnapshot{timestamp: time.Unix(0, 0), regionID: 1, writtenBytesSinceOpen: 100, memtableSize: 100}
	current := regionSnapshot{timestamp: time.Unix(10, 0), regionID: 1, writtenBytesSinceOpen: 10, memtableSize: 20}
	event := regionEvent(benchmarkEvent{}, current, true, previous)
	if !event.CounterReset || !event.MemtableDecreased || event.MemtableWriteBPSApprox != 0 {
		t.Fatalf("unexpected region event: %#v", event)
	}
}
