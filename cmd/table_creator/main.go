package main

import (
	"log"

	"metrics-bench-suite/pkg/cmd/table_creator"
)

func main() {

	var rootCmd = tablecreator.NewCommand()
	if err := rootCmd.Execute(); err != nil {
		log.Fatalf("Error: %v", err)
	}
}
