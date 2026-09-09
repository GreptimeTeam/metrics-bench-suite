package sampleloader

import (
	"strings"
	"testing"
	"time"
)

func TestWriteStatsSnapshotCalculatesWindowAndCumulativeMetrics(t *testing.T) {
	startedAt := time.Date(2026, time.September, 9, 12, 0, 0, 0, time.UTC)
	stats := newWriteStats(startedAt)
	stats.enableChurn(3)
	for i := 1; i <= 100; i++ {
		stats.record(10, time.Duration(i)*time.Millisecond)
	}

	snapshot := stats.snapshot(startedAt.Add(2 * time.Second))
	if snapshot.totalRows != 1000 || snapshot.churnEpoch != 3 {
		t.Fatalf("unexpected totals: rows=%d churn_epoch=%d", snapshot.totalRows, snapshot.churnEpoch)
	}
	if snapshot.averageLatency != 50*time.Millisecond+500*time.Microsecond {
		t.Fatalf("average latency = %s, want 50.5ms", snapshot.averageLatency)
	}
	if snapshot.p99Latency != 99*time.Millisecond {
		t.Fatalf("p99 latency = %s, want 99ms", snapshot.p99Latency)
	}
	if snapshot.throughput != 500 || snapshot.averageThroughput != 500 {
		t.Fatalf("unexpected throughput: realtime=%f average=%f", snapshot.throughput, snapshot.averageThroughput)
	}

	next := stats.snapshot(startedAt.Add(3 * time.Second))
	if next.throughput != 0 || next.averageThroughput != 1000.0/3.0 {
		t.Fatalf("unexpected next throughput: realtime=%f average=%f", next.throughput, next.averageThroughput)
	}
}

func TestFormatWriteStatsIncludesChurnOnlyWhenEnabled(t *testing.T) {
	snapshot := writeStatsSnapshot{currentTime: time.Unix(0, 0), churnEnabled: true, churnEpoch: 4}
	if output := formatWriteStats(snapshot); !strings.Contains(output, "churn_epoch=4") {
		t.Fatalf("expected churn epoch in output: %s", output)
	}
	snapshot.churnEnabled = false
	if output := formatWriteStats(snapshot); strings.Contains(output, "churn_epoch") {
		t.Fatalf("unexpected churn epoch in output: %s", output)
	}
}
