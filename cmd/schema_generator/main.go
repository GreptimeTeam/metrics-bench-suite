package main

import (
	"log"
	"metrics-bench-suite/pkg/cmd/schema_generator"
)

func main() {
	var rootCmd = schemagenerator.NewCommand()
	if err := rootCmd.Execute(); err != nil {
		log.Fatalf("Error: %v", err)
	}
}
