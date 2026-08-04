package main

import (
	"log"

	"metrics-bench-suite/pkg/cmd/sample_generator"
)

func main() {

	var rootCmd = samplegenerator.NewCommand()

	if err := rootCmd.Execute(); err != nil {
		log.Fatalf("Error: %v", err)
	}
}
