package sampleloader

import (
	"fmt"
	"log"
	"slices"
	"sync"
	"time"
)

const statsReportInterval = time.Second

type writeStats struct {
	mu                   sync.Mutex
	startedAt            time.Time
	lastSnapshotAt       time.Time
	totalRows            uint64
	intervalRows         uint64
	intervalLatencyTotal time.Duration
	intervalLatencies    []time.Duration
	churnEnabled         bool
	churnEpoch           int64
	reportEmptyFinal     bool
}

type writeStatsSnapshot struct {
	currentTime       time.Time
	totalRows         uint64
	churnEnabled      bool
	churnEpoch        int64
	averageLatency    time.Duration
	p99Latency        time.Duration
	throughput        float64
	averageThroughput float64
	hasRequests       bool
	reportEmptyFinal  bool
}

func newWriteStats(startedAt time.Time) *writeStats {
	return &writeStats{
		startedAt:      startedAt,
		lastSnapshotAt: startedAt,
	}
}

func (s *writeStats) record(rows uint64, latency time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.totalRows += rows
	s.intervalRows += rows
	s.intervalLatencyTotal += latency
	s.intervalLatencies = append(s.intervalLatencies, latency)
}

func (s *writeStats) setChurnEpoch(epoch int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.churnEpoch = epoch
}

func (s *writeStats) enableChurn(epoch int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.churnEnabled = true
	s.churnEpoch = epoch
}

func (s *writeStats) enableEmptyFinalReport() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reportEmptyFinal = true
}

func (s *writeStats) snapshot(now time.Time) writeStatsSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()

	snapshot := writeStatsSnapshot{
		currentTime:       now,
		totalRows:         s.totalRows,
		churnEnabled:      s.churnEnabled,
		churnEpoch:        s.churnEpoch,
		throughput:        rate(s.intervalRows, now.Sub(s.lastSnapshotAt)),
		averageThroughput: rate(s.totalRows, now.Sub(s.startedAt)),
		hasRequests:       len(s.intervalLatencies) > 0,
		reportEmptyFinal:  s.reportEmptyFinal,
	}
	if snapshot.hasRequests {
		snapshot.averageLatency = s.intervalLatencyTotal / time.Duration(len(s.intervalLatencies))
		slices.Sort(s.intervalLatencies)
		index := (99*len(s.intervalLatencies)+99)/100 - 1
		snapshot.p99Latency = s.intervalLatencies[index]
	}

	s.intervalRows = 0
	s.intervalLatencyTotal = 0
	s.intervalLatencies = s.intervalLatencies[:0]
	s.lastSnapshotAt = now
	return snapshot
}

func rate(rows uint64, elapsed time.Duration) float64 {
	if elapsed <= 0 {
		return 0
	}
	return float64(rows) / elapsed.Seconds()
}

func formatWriteStats(snapshot writeStatsSnapshot) string {
	churn := ""
	if snapshot.churnEnabled {
		churn = fmt.Sprintf(" churn_epoch=%d", snapshot.churnEpoch)
	}
	return fmt.Sprintf(
		"time=%s rows_written=%d%s avg_latency=%s p99_latency=%s throughput=%.2f_rows/s avg_throughput=%.2f_rows/s",
		snapshot.currentTime.Format(time.RFC3339), snapshot.totalRows, churn,
		snapshot.averageLatency, snapshot.p99Latency, snapshot.throughput, snapshot.averageThroughput,
	)
}

func startStatsReporter(stats *writeStats) func() {
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(statsReportInterval)
		defer ticker.Stop()
		for {
			select {
			case now := <-ticker.C:
				log.Print(formatWriteStats(stats.snapshot(now)))
			case <-stop:
				snapshot := stats.snapshot(time.Now())
				if snapshot.hasRequests || snapshot.reportEmptyFinal {
					log.Print(formatWriteStats(snapshot))
				}
				return
			}
		}
	}()
	return func() {
		close(stop)
		<-done
	}
}
