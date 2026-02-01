package samples

import "time"

type ChurnEpochGenerator struct {
	startTime time.Time
	interval  time.Duration
}

// NewChurnEpochGenerator creates a new ChurnEpochGenerator with the specified start time and interval.
func NewChurnEpochGenerator(interval time.Duration) *ChurnEpochGenerator {
	startTime := time.Now()
	return &ChurnEpochGenerator{
		startTime: startTime,
		interval:  interval,
	}
}

func (g *ChurnEpochGenerator) GetChurnEpoch() int64 {
	if g.interval == 0 {
		return 0
	}
	elapsed := time.Since(g.startTime)
	return int64(elapsed / g.interval)
}
