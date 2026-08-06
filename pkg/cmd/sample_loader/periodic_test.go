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

func TestAlternatingCaseSuppliesQualifiedFocusedWorkload(t *testing.T) {
	cmd := NewCommand()
	for name, value := range map[string]string{
		"target-physical-table":       "metrics_physical",
		"pressure-high-min-write-bps": "1",
		"baseline-duration":           "0s",
		"burst-active-duration":       "1s",
		"transient-burst-duration":    "1s",
		"burst-period":                "1s",
		"observe-interval":            "1s",
		"autopilot-case":              autopilotCaseAlternating,
		"autopilot-rate-mode":         autopilotRateModeStableRegion,
	} {
		if err := cmd.Flags().Set(name, value); err != nil {
			t.Fatal(err)
		}
	}
	options, err := periodicOptionsFromCommand(cmd, &SampleLoader{DryRun: true, TickInterval: time.Second, Workers: 1})
	if err != nil {
		t.Fatal(err)
	}
	if options.BurstClass != burstClassQualified || !options.usesStableRegionRateMode() {
		t.Fatalf("alternating case must use qualified stable scheduler workload: %#v", options)
	}
}

func TestStableRegionRateModeIsOptInForAutopilotCases(t *testing.T) {
	cmd := NewCommand()
	for name, value := range map[string]string{
		"target-physical-table":       "metrics_physical",
		"pressure-high-min-write-bps": "1",
		"baseline-duration":           "0s",
		"burst-active-duration":       "1s",
		"transient-burst-duration":    "1s",
		"burst-period":                "1s",
		"observe-interval":            "1s",
		"autopilot-case":              "repartition",
		"autopilot-rate-mode":         "stable-region",
	} {
		if err := cmd.Flags().Set(name, value); err != nil {
			t.Fatal(err)
		}
	}
	options, err := periodicOptionsFromCommand(cmd, &SampleLoader{DryRun: true, TickInterval: time.Second, Workers: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !options.usesStableRegionRateMode() || options.AutopilotBatchSamples != 1000 {
		t.Fatalf("stable-region mode must be enabled for a scheduler case: %#v", options)
	}
	options.AutopilotCase = ""
	if options.usesStableRegionRateMode() {
		t.Fatal("stable-region pacing must not change non-case periodic workloads")
	}
}

func TestStableRegionRateModeBoundsRemoteWriteBatchSize(t *testing.T) {
	if got := stableRegionBatchSamples(20_000, 1_000, 0); got != 1_000 {
		t.Fatalf("stable scheduler mode must bound default batch size, got %d", got)
	}
	if got := stableRegionBatchSamples(500, 1_000, 0); got != 500 {
		t.Fatalf("stable scheduler mode must preserve a caller's smaller batch, got %d", got)
	}
	if got := stableRegionBatchSamples(20_000, 1_000, 128); got != 128 {
		t.Fatalf("repartition scheduler mode must keep the stream continuous, got %d", got)
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
	event := plannedBurstEvent(benchmarkEvent{}, 2, scheduled, periodicOptions{BurstActiveDuration: time.Minute, TransientBurstDuration: time.Second, BurstPeriod: time.Hour, BurstJitter: 0.2, PressureHighMinWriteBPS: 1024, BaselineTargetWriteBPS: 256, AutopilotSchedule: defaultAutopilotSchedule()}, 42, autopilotCaseSelection{caseKind: autopilotCaseRepartition, desiredCase: autopilotCaseRepartition, nextCase: autopilotCaseMigration, sequence: 2}, burstPlan{class: burstClassQualified, duration: time.Minute, targetWriteBPS: 1536, maximumWriteBPS: 3072, pressureScale: 1.0}, hotTarget{regionID: 7, labelName: "namespace", labelValue: "app-1"}, true, 16, 128, &regionSnapshotBundle{snapshots: []regionSnapshot{{regionID: 1, leader: "node-a"}, {regionID: 2, leader: "node-b"}, {regionID: 3, leader: "node-a"}}})
	if event.EventType != "pressure_scheduled" || event.Phase != "baseline" || event.Cycle != 2 || event.ScheduledTSMS != scheduled.UnixMilli() {
		t.Fatalf("unexpected planned event: %#v", event)
	}
	if !strings.Contains(event.Details, `"random_seed":42`) || !strings.Contains(event.Details, `"next_autopilot_case":"migration"`) || !strings.Contains(event.Details, `"effective_autopilot_batch_samples":128`) || !strings.Contains(event.Details, `"rebalance_topology_ready":true`) {
		t.Fatalf("planned event missing scheduling details: %s", event.Details)
	}
}

func TestAlternatingAutopilotCasesAlternateAfterEligibleTargets(t *testing.T) {
	bundle := &regionSnapshotBundle{
		datanodeCount: 3,
		snapshots:     []regionSnapshot{{regionID: 1, leader: "dn-1"}, {regionID: 2, leader: "dn-1"}, {regionID: 3, leader: "dn-1"}, {regionID: 4, leader: "dn-1"}},
		hotTargets:    []hotTarget{{regionID: 1}, {regionID: 2}, {regionID: 3}, {regionID: 4}},
	}
	scheduler := newAutopilotCaseScheduler(autopilotCaseAlternating)

	first := scheduler.selectCase(bundle)
	if first.caseKind != autopilotCaseRepartition || first.desiredCase != autopilotCaseRepartition || first.sequence != 1 {
		t.Fatalf("first alternating selection = %#v", first)
	}
	scheduler.advance(&first, true)
	if first.nextCase != autopilotCaseMigration {
		t.Fatalf("next case after repartition = %q, want migration", first.nextCase)
	}

	second := scheduler.selectCase(bundle)
	if second.caseKind != autopilotCaseMigration || second.desiredCase != autopilotCaseMigration || second.sequence != 2 {
		t.Fatalf("second alternating selection = %#v", second)
	}
	scheduler.advance(&second, true)
	if second.nextCase != autopilotCaseRepartition {
		t.Fatalf("next case after migration = %q, want repartition", second.nextCase)
	}
}

func TestAlternatingAutopilotCasesRetainMigrationAfterBootstrap(t *testing.T) {
	withoutSurplus := &regionSnapshotBundle{datanodeCount: 3, snapshots: []regionSnapshot{{regionID: 1}, {regionID: 2}, {regionID: 3}}}
	scheduler := newAutopilotCaseScheduler(autopilotCaseAlternating)

	first := scheduler.selectCase(withoutSurplus)
	scheduler.advance(&first, true)
	bootstrap := scheduler.selectCase(withoutSurplus)
	if bootstrap.caseKind != autopilotCaseRepartition || bootstrap.desiredCase != autopilotCaseMigration || !bootstrap.migrationBootstrap {
		t.Fatalf("migration bootstrap selection = %#v", bootstrap)
	}
	scheduler.advance(&bootstrap, true)
	if bootstrap.nextCase != autopilotCaseMigration {
		t.Fatalf("bootstrap must retain pending migration, next = %q", bootstrap.nextCase)
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

func TestStableFocusedCaseFillsBytePacerWithoutWaitingForScrapeTicker(t *testing.T) {
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
		Interval:       time.Second,
		MaxSamples:     10,
		Workers:        2,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	current := time.Now()
	err := loader.runPeriodicWrites(ctx, fileConfigs, &current, samples.NewChurnEpochGenerator(0), time.Now().Add(time.Second), time.Hour, "active", 1, periodicOptions{
		TrafficMode:           "steady",
		AutopilotCase:         "repartition",
		AutopilotRateMode:     autopilotRateModeStableRegion,
		AutopilotBatchSamples: 10,
	}, nil, snapshots, benchmarkEvent{}, &hotTarget{
		regionID: 2, labelName: "namespace", labelValues: []string{"app-1"}, caseValues: []string{"app-0", "app-1"},
	}, burstPlan{class: burstClassQualified, targetWriteBPS: 64 * 1024, maximumWriteBPS: 64 * 1024})
	if err != context.DeadlineExceeded {
		t.Fatalf("focused stable case error = %v, want context deadline", err)
	}
	// The legacy ticker-capped loop emits its initial 32 hot passes, then
	// waits an hour. Stable-region mode must instead keep the byte pacer full.
	if got := requests.Load(); got <= 40 {
		t.Fatalf("expected stable byte-paced writes beyond the initial passes, got %d", got)
	}
}

func TestFocusedCaseRefreshesRoutingLanesAfterMappingChange(t *testing.T) {
	remote := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = io.Copy(io.Discard, request.Body)
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer remote.Close()
	events := make(chan benchmarkEvent, 16)
	monitoring := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		decoder := json.NewDecoder(request.Body)
		for {
			var event benchmarkEvent
			if err := decoder.Decode(&event); err == io.EOF {
				break
			} else if err != nil {
				t.Error(err)
				break
			}
			events <- event
		}
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer monitoring.Close()

	lower, upper := 0.0, 1.0
	fileConfigs := []samples.FileConfig{{
		Name: "representative",
		Config: samples.Config{
			Tags:   []samples.Tag{{Name: "namespace", Dist: samples.Distribution{Type: "weighted_preset", Preset: []samples.PresetItem{{Value: "app-0", Weight: 1}, {Value: "app-1", Weight: 1}}}}},
			Fields: []samples.Field{{Name: "value", Dist: samples.Distribution{Type: "uniform", LowerBound: &lower, UpperBound: &upper}}},
		},
	}}
	snapshots := &regionSnapshotStore{}
	snapshots.set(&regionSnapshotBundle{mappingVersion: 1, mappingFingerprint: "before", hotTargets: []hotTarget{
		{regionID: 1, labelName: "namespace", labelValues: []string{"app-0"}},
		{regionID: 2, labelName: "namespace", labelValues: []string{"app-1"}},
	}})
	time.AfterFunc(30*time.Millisecond, func() {
		snapshots.set(&regionSnapshotBundle{mappingVersion: 2, mappingFingerprint: "after", hotTargets: []hotTarget{
			{regionID: 1, labelName: "namespace", labelValues: []string{"app-0"}},
			{regionID: 3, labelName: "namespace", labelValues: []string{"app-1"}},
		}})
	})
	loader := &SampleLoader{RemoteWriteURL: remote.URL, Interval: time.Millisecond, MaxSamples: 10, Workers: 2}
	writer := newEventWriter(periodicOptions{MonitoringURL: monitoring.URL + "/v1/ingest"})
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Millisecond)
	defer cancel()
	current := time.Now()
	err := loader.runPeriodicWrites(ctx, fileConfigs, &current, samples.NewChurnEpochGenerator(0), time.Now().Add(time.Second), time.Hour, "active", 1, periodicOptions{
		TrafficMode:           "steady",
		AutopilotCase:         autopilotCaseRepartition,
		AutopilotRateMode:     autopilotRateModeStableRegion,
		AutopilotBatchSamples: 10,
	}, writer, snapshots, benchmarkEvent{RunID: "run-1"}, &hotTarget{regionID: 2, labelName: "namespace", labelValues: []string{"app-1"}, caseValues: []string{"app-1"}}, burstPlan{class: burstClassQualified, targetWriteBPS: 64 * 1024, maximumWriteBPS: 64 * 1024})
	if err != context.DeadlineExceeded {
		t.Fatalf("focused routing refresh error = %v, want context deadline", err)
	}
	if err := writer.close(context.Background()); err != nil {
		t.Fatal(err)
	}
	for len(events) > 0 {
		event := <-events
		if event.EventType == "workload_routes_refreshed" && strings.Contains(event.Details, `"current_mapping_version":2`) && strings.Contains(event.Details, `"hot_region_ids":[3]`) {
			return
		}
	}
	t.Fatal("expected a flushed workload_routes_refreshed event with remapped region")
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
	if want := 8 * 45 * time.Second; plan.duration != want {
		t.Fatalf("qualified case duration = %s, want %s", plan.duration, want)
	}
	if legacy := newBurstPlan(periodicOptions{BurstActiveDuration: time.Minute}, burstClassQualified, mathrand.New(mathrand.NewSource(1))); legacy.duration != time.Minute {
		t.Fatalf("legacy qualified duration = %s, want configured value", legacy.duration)
	}
}

func TestMigrationPlanReservesRegionBalancerSourceTrigger(t *testing.T) {
	options := periodicOptions{AutopilotSchedule: defaultAutopilotSchedule()}
	plan := qualifyMigrationPlan(options, burstPlan{class: burstClassQualified, targetWriteBPS: 1, maximumWriteBPS: 1}, autopilotCaseMigration)
	want := float64(4*1024*1024) * 1.32
	if math.Abs(plan.targetWriteBPS-want) > 1 {
		t.Fatalf("migration target = %f, want %f", plan.targetWriteBPS, want)
	}
	if plan.maximumWriteBPS < plan.targetWriteBPS {
		t.Fatalf("migration maximum = %f, target = %f", plan.maximumWriteBPS, plan.targetWriteBPS)
	}
	if unchanged := qualifyMigrationPlan(options, burstPlan{class: burstClassQualified, targetWriteBPS: 1}, autopilotCaseRepartition); unchanged.targetWriteBPS != 1 {
		t.Fatalf("repartition plan must stay unchanged: %#v", unchanged)
	}
}

func TestStableMigrationPlanCapsGenericPressureAroundSourceDerivedFloor(t *testing.T) {
	options := periodicOptions{
		AutopilotCase:     autopilotCaseMigration,
		AutopilotRateMode: autopilotRateModeStableRegion,
		AutopilotSchedule: defaultAutopilotSchedule(),
	}
	plan := qualifyMigrationPlan(options, burstPlan{
		class:           burstClassQualified,
		targetWriteBPS:  18 * 1024 * 1024,
		maximumWriteBPS: 24 * 1024 * 1024,
		pressureScale:   0.8,
	}, autopilotCaseMigration)
	base := float64(4*1024*1024) * 1.32
	if want := base * 0.8; math.Abs(plan.targetWriteBPS-want) > 1 {
		t.Fatalf("stable migration target = %f, want %f", plan.targetWriteBPS, want)
	}
	if want := base * maxPressureScale; math.Abs(plan.maximumWriteBPS-want) > 1 {
		t.Fatalf("stable migration maximum = %f, want %f", plan.maximumWriteBPS, want)
	}
}

func TestRepartitionPlanCapsHighGenericBurstForStableHistory(t *testing.T) {
	plan := qualifyRepartitionPlan(burstPlan{class: burstClassQualified, targetWriteBPS: 12 * 1024 * 1024, maximumWriteBPS: 24 * 1024 * 1024}, autopilotCaseRepartition)
	if plan.targetWriteBPS != stableRepartitionTargetBPS || plan.maximumWriteBPS != stableRepartitionTargetBPS*maxPressureScale {
		t.Fatalf("repartition plan must cap the generic burst: %#v", plan)
	}
	if plan.schedulerBatchSamples != stableRepartitionBatchSamples {
		t.Fatalf("repartition scheduler batch size = %d, want %d", plan.schedulerBatchSamples, stableRepartitionBatchSamples)
	}
	varied := qualifyRepartitionPlan(burstPlan{class: burstClassQualified, targetWriteBPS: 12 * 1024 * 1024, maximumWriteBPS: 24 * 1024 * 1024, pressureScale: 0.8}, autopilotCaseRepartition)
	if varied.targetWriteBPS != stableRepartitionTargetBPS*0.8 {
		t.Fatalf("repartition plan must retain seeded variation: %#v", varied)
	}
	if unchanged := qualifyRepartitionPlan(burstPlan{class: burstClassQualified, targetWriteBPS: 1}, autopilotCaseMigration); unchanged.targetWriteBPS != 1 {
		t.Fatalf("migration plan must stay unchanged: %#v", unchanged)
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

func TestStableRegionCaseRaisesWorkerFloorWithoutChangingLegacyWorkers(t *testing.T) {
	target := &hotTarget{labelName: "namespace", caseValues: []string{"app-1", "app-2", "app-3"}}
	stable := periodicOptions{AutopilotCase: autopilotCaseMigration, AutopilotRateMode: autopilotRateModeStableRegion}
	if got, want := schedulerWorkerCount(4, stable, target, 5), 5+3*stableTargetWorkers; got != want {
		t.Fatalf("stable worker floor = %d, want %d", got, want)
	}
	if got := schedulerWorkerCount(21, stable, target, 5); got != 21 {
		t.Fatalf("stable worker count must retain a larger user value, got %d", got)
	}
	if got := schedulerWorkerCount(4, periodicOptions{AutopilotCase: autopilotCaseMigration}, target, 5); got != 4 {
		t.Fatalf("legacy worker count = %d, want configured value", got)
	}
	if got := schedulerWorkerCount(4, stable, nil, 5); got != 4 {
		t.Fatalf("baseline worker count = %d, want configured value", got)
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
	withSurplus := &regionSnapshotBundle{
		datanodeCount: 3,
		snapshots:     []regionSnapshot{{regionID: 1, leader: "dn-1"}, {regionID: 2, leader: "dn-1"}, {regionID: 3, leader: "dn-1"}, {regionID: 4, leader: "dn-1"}},
		hotTargets:    []hotTarget{{regionID: 1}, {regionID: 2}, {regionID: 3}, {regionID: 4}},
	}
	if got := effectiveAutopilotCase("migration", withSurplus); got != "migration" {
		t.Fatalf("migration case with surplus = %q, want migration", got)
	}
}

func TestMigrationCaseBootstrapsRepartitionWithoutColocatedHotRegions(t *testing.T) {
	bundle := &regionSnapshotBundle{
		datanodeCount: 3,
		snapshots:     []regionSnapshot{{regionID: 1, leader: "dn-1"}, {regionID: 2, leader: "dn-1"}, {regionID: 3, leader: "dn-2"}, {regionID: 4, leader: "dn-3"}},
		hotTargets:    []hotTarget{{regionID: 1}, {regionID: 2}, {regionID: 3}, {regionID: 4}},
	}
	if got := effectiveAutopilotCase(autopilotCaseMigration, bundle); got != autopilotCaseRepartition {
		t.Fatalf("migration case without colocated hot regions = %q, want repartition bootstrap", got)
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
		datanodeCount: 2,
		hotTargets: []hotTarget{
			{regionID: 1, labelName: "namespace", labelValues: []string{"app-0"}},
			{regionID: 2, labelName: "namespace", labelValues: []string{"app-1"}},
			{regionID: 3, labelName: "namespace", labelValues: []string{"app-2"}},
			{regionID: 4, labelName: "namespace", labelValues: []string{"app-3"}},
		},
		snapshots: []regionSnapshot{
			{regionID: 1, leader: "dn-1"},
			{regionID: 2, leader: "dn-1"},
			{regionID: 3, leader: "dn-1"},
			{regionID: 4, leader: "dn-2"},
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

func TestMigrationTargetUsesDatanodeCountColocatedRegions(t *testing.T) {
	bundle := &regionSnapshotBundle{
		datanodeCount: 3,
		hotTargets: []hotTarget{
			{regionID: 1, labelName: "namespace", labelValues: []string{"app-0"}},
			{regionID: 2, labelName: "namespace", labelValues: []string{"app-1"}},
			{regionID: 3, labelName: "namespace", labelValues: []string{"app-2"}},
			{regionID: 4, labelName: "namespace", labelValues: []string{"app-3"}},
		},
		snapshots: []regionSnapshot{
			{regionID: 1, leader: "dn-1"},
			{regionID: 2, leader: "dn-1"},
			{regionID: 3, leader: "dn-1"},
			{regionID: 4, leader: "dn-2"},
		},
	}
	if !migrationTargetReady(bundle) {
		t.Fatal("three colocated regions must be a migration-ready source on three datanodes")
	}
	target, ok := selectHotTarget(bundle, mathrand.New(mathrand.NewSource(1)), autopilotCaseMigration)
	if !ok || !reflect.DeepEqual(hotTargetRegionIDs(target), []uint64{1, 2, 3}) {
		t.Fatalf("migration target = %#v, ok=%t; want colocated regions [1 2 3]", target, ok)
	}
}

func TestBalancedMigrationTargetsPreferComparableConfiguredVolume(t *testing.T) {
	group := []hotTarget{{regionID: 1}, {regionID: 2}, {regionID: 3}, {regionID: 4}}
	got := balancedMigrationTargets(group, []sharedpartition.RegionDistribution{
		{RegionID: 1, SeriesCount: 10},
		{RegionID: 2, SeriesCount: 1_000},
		{RegionID: 3, SeriesCount: 900},
		{RegionID: 4, SeriesCount: 1_100},
	}, 3)
	if ids := hotTargetRegionIDs(hotTarget{members: got}); !reflect.DeepEqual(ids, []uint64{2, 3, 4}) {
		t.Fatalf("balanced migration regions = %v, want [2 3 4]", ids)
	}
}

func TestRequiredMigrationTargetCountFitsOneAverageSizedRegion(t *testing.T) {
	if got := requiredMigrationTargetCount(&regionSnapshotBundle{datanodeCount: 3}); got != 3 {
		t.Fatalf("migration target count = %d, want 3", got)
	}
	if got := requiredMigrationTargetCount(nil); got != 2 {
		t.Fatalf("nil migration target count = %d, want fallback 2", got)
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

func TestCaseBackgroundPlanPrefersSmallestSufficientConfigForStableSampling(t *testing.T) {
	fileConfigs := []samples.FileConfig{
		{Name: "small", SeriesCount: 1, FieldGenerators: map[string]samples.FloatGenerator{}, Config: samples.Config{Tags: []samples.Tag{{Name: "namespace", Dist: samples.Distribution{Type: "weighted_preset", Preset: []samples.PresetItem{{Value: "app-0", Weight: 1}}}}}}},
		{Name: "sufficient", SeriesCount: stableBackgroundMinSeries, FieldGenerators: map[string]samples.FloatGenerator{}, Config: samples.Config{Tags: []samples.Tag{{Name: "namespace", Dist: samples.Distribution{Type: "weighted_preset", Preset: []samples.PresetItem{{Value: "app-0", Weight: 1}}}}}}},
		{Name: "large", SeriesCount: 100, FieldGenerators: map[string]samples.FloatGenerator{}, Config: samples.Config{Tags: []samples.Tag{{Name: "namespace", Dist: samples.Distribution{Type: "weighted_preset", Preset: []samples.PresetItem{{Value: "app-0", Weight: 1}}}}}}},
	}
	backgrounds := caseBackgroundPlan(fileConfigs, &regionSnapshotBundle{hotTargets: []hotTarget{{regionID: 1, labelName: "namespace", labelValues: []string{"app-0"}}}})
	if len(backgrounds) != 1 || backgrounds[0].fileConfig.Name != "sufficient" {
		t.Fatalf("expected smallest sufficient representative config, got %#v", backgrounds)
	}
	if backgrounds[0].fileConfig.FieldGenerators != nil {
		t.Fatal("background config must not share mutable field-generator cache")
	}
	configs := schedulerCaseFileConfigs(fileConfigs, hotTarget{regionID: 1, labelName: "namespace", labelValue: "app-0"}, "app-0")
	if len(configs) != 1 || configs[0].Name != "sufficient" || configs[0].FieldGenerators != nil {
		t.Fatalf("scheduler case must use one immutable representative config: %#v", configs)
	}
	fallbacks := caseBackgroundPlan(fileConfigs[:1], &regionSnapshotBundle{hotTargets: []hotTarget{{regionID: 1, labelName: "namespace", labelValues: []string{"app-0"}}}})
	if len(fallbacks) != 1 || fallbacks[0].fileConfig.Name != "small" {
		t.Fatalf("background selection must retain the smallest compatible fallback: %#v", fallbacks)
	}
}

func TestRepartitionCaseSelectsSpreadStableValues(t *testing.T) {
	target := hotTarget{
		regionID:    7,
		labelName:   "namespace",
		labelValues: []string{"app-0", "app-1", "app-2", "app-3", "app-4", "app-5"},
	}
	values := chooseRepartitionTargetValues(target, mathrand.New(mathrand.NewSource(4)))
	if len(values) != 3 || values[0] == values[1] || values[1] == values[2] {
		t.Fatalf("repartition values = %v, want three spread values", values)
	}
	target.caseValues = values
	passes := stableTargetValuePasses(target, 3)
	if got := selectedTargetValueSets(target); !reflect.DeepEqual(got, map[uint64][]string{7: values}) {
		t.Fatalf("selected split input = %#v", got)
	}
	if len(passes) != 3 || passes[0].value == passes[1].value || passes[1].value == passes[2].value {
		t.Fatalf("repartition must retain all selected values in each stable cycle: %#v", passes)
	}
}

func TestSelectHotTargetKeepsMultipleSplitValuesForRepartition(t *testing.T) {
	bundle := &regionSnapshotBundle{hotTargets: []hotTarget{{
		regionID:    7,
		labelName:   "namespace",
		labelValues: []string{"app-0", "app-1", "app-2", "app-3"},
	}}}
	target, ok := selectHotTarget(bundle, mathrand.New(mathrand.NewSource(9)), autopilotCaseRepartition)
	if !ok || len(target.caseValues) != 3 || target.labelValue != target.caseValues[0] {
		t.Fatalf("repartition target must retain three stable split values: %#v, ok=%t", target, ok)
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

func TestRegionSnapshotStoreSignalsOnlyRoutingChanges(t *testing.T) {
	store := &regionSnapshotStore{}
	store.set(&regionSnapshotBundle{mappingVersion: 1, mappingFingerprint: "one"})
	signal := store.mappingChangeSignal(1)
	store.set(&regionSnapshotBundle{mappingVersion: 1, mappingFingerprint: "one", timestamp: time.Now()})
	select {
	case <-signal:
		t.Fatal("ordinary snapshot refresh must not restart routing producers")
	default:
	}
	store.set(&regionSnapshotBundle{mappingVersion: 2, mappingFingerprint: "two"})
	select {
	case <-signal:
	case <-time.After(time.Second):
		t.Fatal("routing change did not signal active workload")
	}
	select {
	case <-store.mappingChangeSignal(1):
	default:
		t.Fatal("a caller with an old mapping version must observe the completed change")
	}
}

func TestRegionMappingFingerprintIgnoresStatisticsButTracksRouting(t *testing.T) {
	regions := []regionSnapshot{{regionID: 1, leader: "dn-0", partitionExpression: "namespace", partitionDescription: "< app-1", memtableSize: 10}}
	targets := []hotTarget{{regionID: 1, labelName: "namespace", labelValues: []string{"app-0"}}}
	first := regionMappingFingerprint(regions, targets)
	regions[0].memtableSize = 20
	regions[0].writtenBytesSinceOpen = 100
	if got := regionMappingFingerprint(regions, targets); got != first {
		t.Fatalf("statistics-only refresh changed routing fingerprint: %s != %s", got, first)
	}
	regions[0].leader = "dn-1"
	if got := regionMappingFingerprint(regions, targets); got == first {
		t.Fatal("leader change must change routing fingerprint")
	}
}

func TestRemapHotTargetSplitsSelectedValuesAcrossNewRegions(t *testing.T) {
	original := hotTarget{regionID: 1, labelName: "namespace", labelValues: []string{"app-0", "app-1", "app-2"}, caseValues: []string{"app-0", "app-1", "app-2"}}
	remapped, ok := remapHotTarget(&regionSnapshotBundle{hotTargets: []hotTarget{
		{regionID: 2, labelName: "namespace", labelValues: []string{"app-0"}},
		{regionID: 3, labelName: "namespace", labelValues: []string{"app-1", "app-2"}},
	}}, original)
	if !ok {
		t.Fatal("expected selected values to remap to child regions")
	}
	if got, want := selectedTargetValueSets(remapped), map[uint64][]string{2: {"app-0"}, 3: {"app-1", "app-2"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("remapped values = %#v, want %#v", got, want)
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

func TestRegionStabilityEventRecordsCounterRateAndFlushContext(t *testing.T) {
	refresher := regionRefresher{
		rateHistory: make(map[uint64][]regionRateSample),
		schedule: autopilotSchedule{
			maxHistoryWindows:  2,
			repartitionSamples: 3,
			maxRegionHistoryCV: 0.2,
		},
	}
	previous := regionSnapshot{timestamp: time.Unix(0, 0), regionID: 42, leader: "node-a", writtenBytesSinceOpen: 100, memtableSize: 100}
	current := regionSnapshot{timestamp: time.Unix(10, 0), regionID: 42, leader: "node-a", writtenBytesSinceOpen: 400, memtableSize: 20}
	event := refresher.regionStabilityEvent(current, previous, true)
	if event.EventType != "region_stability_window" || !event.MemtableDecreased || event.WrittenBytesBPS != 30 {
		t.Fatalf("unexpected stability event: %#v", event)
	}
	var details map[string]any
	if err := json.Unmarshal([]byte(event.Details), &details); err != nil {
		t.Fatal(err)
	}
	if details["throughput_source"] != "written_bytes_since_open_delta" || details["scheduler_write_metric"] != "metasrv_memtable_size_delta_ewma" {
		t.Fatalf("missing throughput provenance: %#v", details)
	}
	if details["sample_count"] != float64(1) || details["stable_estimate"] != false || details["memtable_flush_observed"] != true {
		t.Fatalf("unexpected stability details: %#v", details)
	}

	leaderMoved := current
	leaderMoved.timestamp = leaderMoved.timestamp.Add(10 * time.Second)
	leaderMoved.leader = "node-b"
	leaderMoved.writtenBytesSinceOpen += 300
	event = refresher.regionStabilityEvent(leaderMoved, current, true)
	if err := json.Unmarshal([]byte(event.Details), &details); err != nil {
		t.Fatal(err)
	}
	if details["history_reset_reason"] != "leader_changed" || details["sample_count"] != float64(0) {
		t.Fatalf("leader change must reset the estimated history: %#v", details)
	}
}

func TestParseAutopilotScheduleReadsRegionHistoryCV(t *testing.T) {
	schedule := parseAutopilotSchedule("min_samples = 4\nmax_region_history_cv = 0.35", defaultAutopilotSchedule())
	if schedule.repartitionSamples != 4 || schedule.maxRegionHistoryCV != 0.35 {
		t.Fatalf("unexpected schedule: %#v", schedule)
	}
}

func TestParseAutopilotScheduleReadsRegionBalancerLoadSettings(t *testing.T) {
	schedule := parseAutopilotSchedule("min_load_threshold = '6MiB'\nacceptable_load_ratio = 0.2", defaultAutopilotSchedule())
	if schedule.rebalanceMinLoadBPS != 6*1024*1024 || schedule.rebalanceAcceptableLoadRatio != 0.2 {
		t.Fatalf("unexpected rebalance schedule: %#v", schedule)
	}
}

func TestStableTargetValuePassesRepeatOneConfiguredValueWithinCase(t *testing.T) {
	target := hotTarget{
		regionID:    1109,
		labelName:   "namespace",
		labelValue:  "app-27",
		labelValues: []string{"app-26", "app-27"},
	}
	passes := stableTargetValuePasses(target, 4)
	if len(passes) != 4 {
		t.Fatalf("unexpected pass count: %#v", passes)
	}
	for _, pass := range passes {
		if pass.target.regionID != target.regionID || pass.target.labelName != target.labelName || pass.value != "app-27" {
			t.Fatalf("unexpected target pass: %#v", pass)
		}
	}

	passes = stableTargetValuePasses(target, 1)
	if len(passes) != 1 || passes[0].value != "app-27" {
		t.Fatalf("a stable scheduler target must retain one selected value: %#v", passes)
	}
	if got := chooseStableTargetValue(target, mathrand.New(mathrand.NewSource(1))); got != "app-27" {
		t.Fatalf("seeded value selection = %q, want app-27", got)
	}
}

func TestStableTargetValuePassesCoverMigrationMembers(t *testing.T) {
	target := hotTarget{members: []hotTarget{
		{regionID: 1108, labelName: "namespace", labelValue: "app-23", labelValues: []string{"app-23", "app-24"}},
		{regionID: 1109, labelName: "namespace", labelValue: "app-27", labelValues: []string{"app-26", "app-27"}},
	}}
	passes := stableTargetValuePasses(target, 2)
	if len(passes) != 2 {
		t.Fatalf("migration case must cover every selected member value: %#v", passes)
	}
	got := make(map[uint64][]string)
	for _, pass := range passes {
		got[pass.target.regionID] = append(got[pass.target.regionID], pass.value)
	}
	if !reflect.DeepEqual(got, map[uint64][]string{1108: {"app-23"}, 1109: {"app-27"}}) {
		t.Fatalf("unexpected migration value coverage: %#v", got)
	}
	if got := selectedTargetLabelValues(target); !reflect.DeepEqual(got, map[uint64]string{1108: "app-23", 1109: "app-27"}) {
		t.Fatalf("unexpected selected target values: %#v", got)
	}
}

func TestStableRegionRateModeDisablesAdaptivePressure(t *testing.T) {
	pacer := newBytePacer(1024)
	plan := burstPlan{class: burstClassQualified}
	if !adaptivePressureEnabled(periodicOptions{AutopilotRateMode: autopilotRateModeLegacy, AutopilotCase: "repartition"}, "active", plan, workloadMetrics{}, pacer) {
		t.Fatal("legacy scheduler case must retain adaptive pressure behavior")
	}
	if adaptivePressureEnabled(periodicOptions{AutopilotRateMode: autopilotRateModeStableRegion, AutopilotCase: "repartition"}, "active", plan, workloadMetrics{}, pacer) {
		t.Fatal("stable-region scheduler case must preserve its planned per-region rate")
	}
}
