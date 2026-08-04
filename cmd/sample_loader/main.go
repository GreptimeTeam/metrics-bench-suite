package main

import (
	"log"
	"metrics-bench-suite/pkg/cmd/sample_loader"
)

func main() {
	var rootCmd = sampleloader.NewCommand()
	if err := rootCmd.Execute(); err != nil {
		log.Fatalf("Error: %v", err)
	}
}
