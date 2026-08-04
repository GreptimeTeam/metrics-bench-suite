package main

import (
	"log"

	"metrics-bench-suite/pkg/cmd/remote_write_request_viewer"
)

func main() {

	var rootCmd = remotewriterequestviewer.NewCommand()

	if err := rootCmd.Execute(); err != nil {
		log.Fatal(err)
	}
}
