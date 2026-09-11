package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"metrics-bench-suite/pkg/dataset"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := command().ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func printJSON(value any) error {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func command() *cobra.Command {
	root := &cobra.Command{Use: "metrics_dataset", Short: "Inspect, generate, and verify reusable historical metrics datasets", SilenceUsage: true, SilenceErrors: true}
	version := &cobra.Command{Use: "version", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
		value, err := dataset.Identity()
		if err != nil {
			return err
		}
		return printJSON(value)
	}}
	root.AddCommand(version)
	for _, action := range []string{"inspect", "generate"} {
		var config, profile, profilesDir, output, start, end string
		var interval, churnInterval time.Duration
		options := dataset.Options{}
		cmd := &cobra.Command{Use: action, Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
			if (config == "") == (profile == "") {
				return fmt.Errorf("specify exactly one of --config or --profile")
			}
			if profile != "" {
				switch profile {
				case "k8s-small", "k8s-medium", "k8s-large":
				default:
					return fmt.Errorf("unknown profile %q", profile)
				}
				config = filepath.Join(profilesDir, profile)
			}
			if cmd.Name() == "inspect" {
				inspection, err := dataset.Inspect(config)
				if err != nil {
					return err
				}
				if err := printJSON(inspection); err != nil {
					return err
				}
				if !inspection.Valid {
					return fmt.Errorf("config inspection failed")
				}
				return nil
			}
			var err error
			if options.Start, err = time.Parse(time.RFC3339Nano, start); err != nil {
				return err
			}
			options.Start = options.Start.UTC()
			if options.End, err = time.Parse(time.RFC3339Nano, end); err != nil {
				return err
			}
			options.End = options.End.UTC()
			if interval%time.Millisecond != 0 || churnInterval%time.Millisecond != 0 {
				return fmt.Errorf("intervals must use whole milliseconds")
			}
			options.IntervalMillis = interval.Milliseconds()
			options.ChurnIntervalMillis = churnInterval.Milliseconds()
			options.Profile = profile
			if output == "" {
				return fmt.Errorf("--output-dir is required")
			}
			summary, err := dataset.Generate(cmd.Context(), config, output, options)
			if err != nil {
				return err
			}
			return printJSON(summary)
		}}
		cmd.Flags().StringVar(&config, "config", "", "Metric YAML directory (mutually exclusive with --profile)")
		cmd.Flags().StringVar(&profile, "profile", "", "Curated profile: k8s-small, k8s-medium, k8s-large")
		cmd.Flags().StringVar(&profilesDir, "profiles-dir", "profiles", "Curated profiles directory in metrics-bench-suite")
		if action == "generate" {
			cmd.Flags().StringVar(&output, "output-dir", "", "New or empty output directory")
			cmd.Flags().StringVar(&start, "start-date", "2025-03-21T09:45:00Z", "Inclusive RFC3339 start")
			cmd.Flags().StringVar(&end, "end-date", "2025-03-21T10:21:00Z", "Exclusive RFC3339 end")
			cmd.Flags().DurationVar(&interval, "interval", 30*time.Second, "Sample interval")
			cmd.Flags().Uint64Var(&options.Seed, "seed", 123456, "Deterministic random seed")
			cmd.Flags().IntVar(&options.Replica, "replica", 0, "Replica label value")
			cmd.Flags().Float64Var(&options.ChurnRate, "churn-rate", 0, "Fraction of base series whose identity changes each epoch")
			cmd.Flags().DurationVar(&churnInterval, "churn-interval", 0, "Churn interval in dataset time")
			cmd.Flags().IntVar(&options.MaxSamples, "max-samples-per-request", 10000, "Maximum samples per output request")
		}
		root.AddCommand(cmd)
	}
	var input string
	verify := &cobra.Command{Use: "verify", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
		if input == "" {
			return fmt.Errorf("--data-dir is required")
		}
		summary, err := dataset.Verify(cmd.Context(), input)
		if err != nil {
			return err
		}
		return printJSON(summary)
	}}
	verify.Flags().StringVar(&input, "data-dir", "", "Dataset directory containing summary.json and remote-write files")
	root.AddCommand(verify)
	return root
}
