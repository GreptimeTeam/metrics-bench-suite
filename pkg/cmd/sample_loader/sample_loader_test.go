package sample_loader

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"metrics-bench-suite/pkg/samples"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/prometheus/prompb"
)

func TestAuthorizationHeader(t *testing.T) {
	s := &SampleLoader{
		Username: "alice",
		Password: "secret",
	}

	expected := "Basic YWxpY2U6c2VjcmV0"
	if got := s.authorizationHeader(); got != expected {
		t.Fatalf("expected authorization header %q, got %q", expected, got)
	}
}

func TestAuthorizationHeaderEmpty(t *testing.T) {
	s := &SampleLoader{}

	if got := s.authorizationHeader(); got != "" {
		t.Fatalf("expected empty authorization header, got %q", got)
	}
}

func TestParseRemoteWriteVersion(t *testing.T) {
	tests := []struct {
		input    string
		expected remoteWriteVersion
	}{
		{input: "v1", expected: remoteWriteV1},
		{input: "v2", expected: remoteWriteV2},
	}

	for _, test := range tests {
		got, err := parseRemoteWriteVersion(test.input)
		if err != nil {
			t.Fatalf("parse %q: %v", test.input, err)
		}
		if got != test.expected {
			t.Fatalf("parse %q: expected %d, got %d", test.input, test.expected, got)
		}
	}

	if _, err := parseRemoteWriteVersion("v3"); err == nil {
		t.Fatal("expected unsupported version error")
	}
}

func TestConvertToRemoteWriteV2Request(t *testing.T) {
	request := prompb.WriteRequest{
		Timeseries: []prompb.TimeSeries{
			{
				Labels: []prompb.Label{
					{Name: "__name__", Value: "cpu_usage"},
					{Name: "instance", Value: "a"},
				},
				Samples: []prompb.Sample{
					{Value: 1, Timestamp: 2},
					{Value: 3, Timestamp: 4},
				},
			},
			{
				Labels: []prompb.Label{
					{Name: "__name__", Value: "cpu_usage"},
					{Name: "instance", Value: "b"},
				},
				Samples: []prompb.Sample{{Value: 5, Timestamp: 6}},
			},
		},
	}

	got := convertToRemoteWriteV2Request(request)
	expectedSymbols := []string{"", "__name__", "cpu_usage", "instance", "a", "b"}
	if !reflect.DeepEqual(got.Symbols, expectedSymbols) {
		t.Fatalf("expected symbols %v, got %v", expectedSymbols, got.Symbols)
	}
	if got.Symbols[0] != "" {
		t.Fatalf("expected empty first symbol, got %q", got.Symbols[0])
	}

	expectedLabelRefs := [][]uint32{{1, 2, 3, 4}, {1, 2, 3, 5}}
	for i := range got.Timeseries {
		if !reflect.DeepEqual(got.Timeseries[i].LabelsRefs, expectedLabelRefs[i]) {
			t.Fatalf("series %d: expected label refs %v, got %v", i, expectedLabelRefs[i], got.Timeseries[i].LabelsRefs)
		}
		if len(got.Timeseries[i].Samples) != len(request.Timeseries[i].Samples) {
			t.Fatalf("series %d: expected %d samples, got %d", i, len(request.Timeseries[i].Samples), len(got.Timeseries[i].Samples))
		}
		for j := range got.Timeseries[i].Samples {
			expected := request.Timeseries[i].Samples[j]
			actual := got.Timeseries[i].Samples[j]
			if actual.Value != expected.Value || actual.Timestamp != expected.Timestamp {
				t.Fatalf("series %d sample %d: expected %+v, got %+v", i, j, expected, actual)
			}
		}
	}
}

func TestTagSetPermutationStream(t *testing.T) {
	// Test case 1: Empty labels
	t.Run("Empty labels", func(t *testing.T) {
		var labels []samples.LabelCandidates
		var results []samples.SeriesWithIndex
		samples.TagSetPermutationStream(labels, func(swi samples.SeriesWithIndex) { results = append(results, swi) })

		expectedCount := 1
		if len(results) != expectedCount {
			t.Errorf("Expected total count %d, got %d", expectedCount, len(results))
		}

		if len(results[0].Series) != 0 {
			t.Errorf("Expected empty series, got %v", results[0].Series)
		}
	})

	// Test case 2: Single label with one value
	t.Run("Single label with one value", func(t *testing.T) {
		labels := []samples.LabelCandidates{
			{
				Name:   "label1",
				Values: []string{"value1"},
			},
		}
		var results []samples.SeriesWithIndex
		samples.TagSetPermutationStream(labels, func(swi samples.SeriesWithIndex) { results = append(results, swi) })

		expectedCount := 1
		if len(results) != expectedCount {
			t.Errorf("Expected total count %d, got %d", expectedCount, len(results))
		}

		expectedSeries := []samples.LabelPair{
			{Name: "label1", Value: "value1"},
		}
		if !reflect.DeepEqual(results[0].Series, expectedSeries) {
			t.Errorf("Expected %v, got %v", expectedSeries, results[0].Series)
		}
	})

	// Test case 3: Single label with multiple values
	t.Run("Single label with multiple values", func(t *testing.T) {
		labels := []samples.LabelCandidates{
			{
				Name:   "label1",
				Values: []string{"value1", "value2", "value3"},
			},
		}
		var results []samples.SeriesWithIndex
		samples.TagSetPermutationStream(labels, func(swi samples.SeriesWithIndex) { results = append(results, swi) })

		expectedCount := 3
		if len(results) != expectedCount {
			t.Errorf("Expected total count %d, got %d", expectedCount, len(results))
		}

		foundValues := make([]string, 0, len(results))
		for _, result := range results {
			foundValues = append(foundValues, result.Series[0].Value)
		}
		sort.Strings(foundValues)

		expectedValues := []string{"value1", "value2", "value3"}
		if !reflect.DeepEqual(foundValues, expectedValues) {
			t.Errorf("Expected values %v, got %v", expectedValues, foundValues)
		}
	})

	// Test case 4: Multiple labels
	t.Run("Multiple labels", func(t *testing.T) {
		labels := []samples.LabelCandidates{
			{
				Name:   "label1",
				Values: []string{"a", "b"},
			},
			{
				Name:   "label2",
				Values: []string{"x", "y"},
			},
		}
		var results []samples.SeriesWithIndex
		samples.TagSetPermutationStream(labels, func(swi samples.SeriesWithIndex) { results = append(results, swi) })

		expectedCount := 4 // 2 * 2 = 4 combinations
		if len(results) != expectedCount {
			t.Errorf("Expected total count %d, got %d", expectedCount, len(results))
		}

		var combinations []string
		for _, result := range results {
			combination := result.Series[0].Value + "," + result.Series[1].Value
			combinations = append(combinations, combination)
		}

		expectedCombinations := []string{
			"a,x",
			"b,x",
			"a,y",
			"b,y",
		}
		if !reflect.DeepEqual(combinations, expectedCombinations) {
			t.Errorf("Expected %v, got %v", expectedCombinations, combinations)
		}
	})

	// Test case 5: Multiple labels with different value counts
	t.Run("Multiple labels with different value counts", func(t *testing.T) {
		labels := []samples.LabelCandidates{
			{
				Name:   "env",
				Values: []string{"prod", "dev"},
			},
			{
				Name:   "region",
				Values: []string{"us-east", "us-west", "eu-west"},
			},
			{
				Name:   "service",
				Values: []string{"web"},
			},
		}
		var results []samples.SeriesWithIndex
		samples.TagSetPermutationStream(labels, func(swi samples.SeriesWithIndex) { results = append(results, swi) })

		expectedCount := 6 // 2 * 3 * 1 = 6 combinations
		if len(results) != expectedCount {
			t.Errorf("Expected total count %d, got %d", expectedCount, len(results))
		}

		var combinations []string
		for _, result := range results {
			combination := result.Series[0].Value + "," + result.Series[1].Value + "," + result.Series[2].Value
			combinations = append(combinations, combination)
		}

		expectedCombinations := []string{
			"prod,us-east,web",
			"dev,us-east,web",
			"prod,us-west,web",
			"dev,us-west,web",
			"prod,eu-west,web",
			"dev,eu-west,web",
		}
		if !reflect.DeepEqual(combinations, expectedCombinations) {
			t.Errorf("Expected %v, got %v", expectedCombinations, combinations)
		}
	})
}

// TestGenerateTimeSeriesForFileConfig verifies that the generated time series meet the requirements:
// 1. First label is always '__name__'
// 2. All labels are sorted according to label name lexicographically
func TestGenerateTimeSeriesForFileConfig(t *testing.T) {
	s := &SampleLoader{}

	// Create distribution values for the test
	envValues := []samples.PresetItem{
		{Value: "prod", Weight: 1},
		{Value: "dev", Weight: 1},
	}

	regionValues := []samples.PresetItem{
		{Value: "us-west", Weight: 1},
		{Value: "us-east", Weight: 1},
	}

	serviceValues := []samples.PresetItem{
		{Value: "web", Weight: 1},
		{Value: "api", Weight: 1},
	}

	// Create a mock file config with tags
	fileConfig := samples.FileConfig{
		Name: "test_metric",
		Config: samples.Config{
			Tags: []samples.Tag{
				{
					Name: "env",
					Dist: samples.Distribution{
						Type:   "weighted_preset",
						Preset: envValues,
					},
				},
				{
					Name: "region",
					Dist: samples.Distribution{
						Type:   "weighted_preset",
						Preset: regionValues,
					},
				},
				{
					Name: "service",
					Dist: samples.Distribution{
						Type:   "weighted_preset",
						Preset: serviceValues,
					},
				},
			},
			Fields: []samples.Field{
				{
					Name: "value",
					Dist: samples.Distribution{
						Type:       "uniform",
						LowerBound: &[]float64{0.0}[0],
						UpperBound: &[]float64{100.0}[0],
					},
				},
			},
		},
		ReplicaInsertIndex: 2,
	}

	t.Run("generateTimeSeries", func(t *testing.T) {
		currentTime := time.Now()
		timeSeriesChan := s.generateTimeSeriesForFileConfig(context.Background(), &fileConfig, currentTime, 0)

		// Collect all time series
		timeSeries := make([]prompb.TimeSeries, 0)
		for ts := range timeSeriesChan {
			timeSeries = append(timeSeries, ts)
		}

		// Verify that there are time series generated
		if len(timeSeries) == 0 {
			t.Error("Expected at least one time series to be generated")
			return
		}

		// For each time series, verify the requirements
		for _, ts := range timeSeries {
			// Check 1: First label is '__name__'
			if len(ts.Labels) == 0 {
				t.Error("Time series has no labels")
				continue
			}

			if ts.Labels[0].Name != "__name__" {
				t.Errorf("First label should be '__name__', got '%s'", ts.Labels[0].Name)
			}

			if ts.Labels[0].Value != "test_metric" {
				t.Errorf("First label value should be 'test_metric', got '%s'", ts.Labels[0].Value)
			}

			// Check 2: Other labels (excluding __name__) are sorted lexicographically
			labelsAfterName := make([]prompb.Label, 0)
			for _, label := range ts.Labels[1:] { // Skip the first __name__ label
				labelsAfterName = append(labelsAfterName, label)
			}

			// Verify labels are sorted lexicographically (excluding __name__)
			for i := 0; i < len(labelsAfterName)-1; i++ {
				if labelsAfterName[i].Name > labelsAfterName[i+1].Name {
					t.Errorf("Labels are not sorted lexicographically: '%s' > '%s'",
						labelsAfterName[i].Name, labelsAfterName[i+1].Name)
				}
			}
		}
	})
}

func TestNewCommandDoesNotExposeDatabaseFlag(t *testing.T) {
	cmd := NewCommand()
	if flag := cmd.Flags().Lookup("database"); flag != nil {
		t.Fatalf("expected no database flag, got %q", flag.Name)
	}
}

func TestRunWithDurationStops(t *testing.T) {
	var logs bytes.Buffer
	previousLogOutput := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(previousLogOutput)

	configPath := filepath.Join(t.TempDir(), "test.yaml")
	config := []byte(`tags:
  - name: instance
    type: string
    dist:
      type: constant_string
      value: test
fields:
  - name: value
    type: float
    dist:
      type: uniform
      lower_bound: 0
      upper_bound: 1
`)
	if err := os.WriteFile(configPath, config, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cmd := NewCommand()
	for name, value := range map[string]string{
		"config":               configPath,
		"dry-run":              "true",
		"duration":             "20ms",
		"interval":             "1s",
		"remote-write-version": "v2",
		"tick-interval":        "50ms",
	} {
		if err := cmd.Flags().Set(name, value); err != nil {
			t.Fatalf("set %s: %v", name, err)
		}
	}

	start := time.Now()
	if err := (&SampleLoader{}).run(cmd, nil); err != nil {
		t.Fatalf("run loader: %v", err)
	}
	if elapsed := time.Since(start); elapsed >= time.Second {
		t.Fatalf("duration run took too long: %s", elapsed)
	}
	for _, expected := range []string{
		"Run statistics:",
		`requests_total:\s+1`,
		`requests_succeeded:\s+1`,
		`requests_failed:\s+0`,
		`samples_total:\s+1`,
		`samples_in_succeeded_requests:\s+1`,
		`samples_in_failed_requests:\s+0`,
		`dry_run:\s+true`,
	} {
		if !regexp.MustCompile(expected).MatchString(logs.String()) {
			t.Fatalf("expected log output to contain %q, got:\n%s", expected, logs.String())
		}
	}
}

func TestDurationStopsBlockedGeneration(t *testing.T) {
	preset := make([]samples.PresetItem, 100)
	for i := range preset {
		preset[i] = samples.PresetItem{Value: fmt.Sprintf("instance-%d", i), Weight: 1}
	}
	fileConfigs := []samples.FileConfig{{
		Name: "test_metric",
		Config: samples.Config{
			Tags: []samples.Tag{{
				Name: "instance",
				Dist: samples.Distribution{Type: "weighted_preset", Preset: preset},
			}},
			Fields: []samples.Field{{
				Name: "value",
				Dist: samples.Distribution{Type: "constant_float", Value: 1.0},
			}},
		},
	}}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	done := make(chan bool, 1)
	go func() {
		done <- (&SampleLoader{MaxSamples: 1}).convertToRemoteWriteRequestsStreaming(
			ctx,
			fileConfigs,
			time.Now(),
			make(chan prompb.WriteRequest),
			0,
		)
	}()

	select {
	case completed := <-done:
		if completed {
			t.Fatal("expected generation to stop at the deadline")
		}
	case <-time.After(time.Second):
		t.Fatal("generation did not stop at the deadline")
	}
}

func TestWorkerCancelsAfterDrainTimeout(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(started)
		<-release
	}))
	defer server.Close()
	defer close(release)

	requestCtx, cancelRequests := context.WithCancel(context.Background())
	defer cancelRequests()
	requestChan := make(chan prompb.WriteRequest, 1)
	requestChan <- prompb.WriteRequest{}
	close(requestChan)

	var (
		wg    sync.WaitGroup
		stats runStats
	)
	wg.Add(1)
	go worker(requestCtx, 0, server.URL, "", remoteWriteV1, requestChan, &wg, false, &stats)

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("worker did not start its request")
	}

	waitForWorkers(&wg, cancelRequests, 20*time.Millisecond)
	if stats.failedRequests != 1 {
		t.Fatalf("expected the canceled request to fail, got %+v", stats)
	}
}

func TestRunRejectsDurationWithInfinite(t *testing.T) {
	cmd := NewCommand()
	for name, value := range map[string]string{
		"dry-run":  "true",
		"duration": "60s",
		"infinite": "true",
	} {
		if err := cmd.Flags().Set(name, value); err != nil {
			t.Fatalf("set %s: %v", name, err)
		}
	}

	err := (&SampleLoader{}).run(cmd, nil)
	if err == nil || !strings.Contains(err.Error(), "cannot be used together") {
		t.Fatalf("expected incompatible flags error, got %v", err)
	}
}

func TestGenerateTimeSeriesForFileConfigReplicaLabel(t *testing.T) {
	s := &SampleLoader{}

	sampleLoaderValue := reflect.ValueOf(s).Elem()
	replicaField := sampleLoaderValue.FieldByName("Replica")
	if !replicaField.IsValid() {
		t.Fatalf("SampleLoader missing Replica field")
	}
	if replicaField.Kind() != reflect.Int {
		t.Fatalf("SampleLoader Replica field should be int")
	}
	replicaField.SetInt(2)

	labelsValues := []samples.PresetItem{
		{Value: "a", Weight: 1},
	}
	fileConfig := samples.FileConfig{
		Name: "test_metric",
		Config: samples.Config{
			Tags: []samples.Tag{
				{
					Name: "alpha",
					Dist: samples.Distribution{
						Type:   "weighted_preset",
						Preset: labelsValues,
					},
				},
				{
					Name: "sigma",
					Dist: samples.Distribution{
						Type:   "weighted_preset",
						Preset: labelsValues,
					},
				},
			},
			Fields: []samples.Field{
				{
					Name: "value",
					Dist: samples.Distribution{
						Type:       "uniform",
						LowerBound: &[]float64{0.0}[0],
						UpperBound: &[]float64{100.0}[0],
					},
				},
			},
		},
		ReplicaInsertIndex: 1,
	}

	currentTime := time.Now()
	timeSeriesChan := s.generateTimeSeriesForFileConfig(context.Background(), &fileConfig, currentTime, 0)

	var timeSeries []prompb.TimeSeries
	for ts := range timeSeriesChan {
		timeSeries = append(timeSeries, ts)
	}
	if len(timeSeries) == 0 {
		t.Fatalf("Expected at least one time series")
	}

	ts := timeSeries[0]
	if len(ts.Labels) < 4 {
		t.Fatalf("Expected at least 4 labels, got %d", len(ts.Labels))
	}

	expectedOrder := []string{"__name__", "alpha", "replica", "sigma"}
	for i, name := range expectedOrder {
		if ts.Labels[i].Name != name {
			t.Fatalf("Expected label %d to be %q, got %q", i, name, ts.Labels[i].Name)
		}
	}

	if ts.Labels[2].Value != "2" {
		t.Fatalf("Expected replica label value to be %q, got %q", "2", ts.Labels[2].Value)
	}
}
