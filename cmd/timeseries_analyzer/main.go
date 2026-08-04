package main

import (
	"log"

	"metrics-bench-suite/pkg/cmd/timeseries_analyzer"
)

func main() {

	var rootCmd = timeseriesanalyzer.NewCommand()

	if err := rootCmd.Execute(); err != nil {
		log.Fatalf("Error processing tcpflow files: %v", err)
	}
}
