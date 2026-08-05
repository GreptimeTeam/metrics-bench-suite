package sampleloader

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestPeriodicOptionsRequireExplicitExpectation(t *testing.T) {
	cmd := NewCommand()
	for name, value := range map[string]string{
		"target-physical-table":       "metrics_physical",
		"pressure-high-min-write-bps": "1",
		"baseline-duration":           "0s",
		"burst-active-duration":       "1s",
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
