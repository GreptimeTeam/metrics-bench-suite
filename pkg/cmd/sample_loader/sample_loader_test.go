package sample_loader

import (
	"fmt"
	"metrics-bench-suite/pkg/samples"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/prometheus/prometheus/prompb"
)

func TestTagSetPermutationStream(t *testing.T) {
	// Test case 1: Empty labels
	t.Run("Empty labels", func(t *testing.T) {
		var labels []samples.LabelCandidates
		permChan := make(chan SeriesWithIndex, 10)
		totalCount := 0

		go TagSetPermutationStream(labels, permChan, &totalCount)

		var results []SeriesWithIndex
		for perm := range permChan {
			results = append(results, perm)
		}

		expectedCount := 1
		if totalCount != expectedCount {
			t.Errorf("Expected total count %d, got %d", expectedCount, totalCount)
		}

		if len(results) != 1 {
			t.Errorf("Expected 1 result, got %d", len(results))
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
		permChan := make(chan SeriesWithIndex, 10)
		totalCount := 0

		go TagSetPermutationStream(labels, permChan, &totalCount)

		results := make([]SeriesWithIndex, 0)
		for perm := range permChan {
			results = append(results, perm)
		}

		expectedCount := 1
		if totalCount != expectedCount {
			t.Errorf("Expected total count %d, got %d", expectedCount, totalCount)
		}

		if len(results) != 1 {
			t.Errorf("Expected 1 result, got %d", len(results))
		}

		expectedSeries := []LabelPair{
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
		permChan := make(chan SeriesWithIndex, 10)
		totalCount := 0

		go TagSetPermutationStream(labels, permChan, &totalCount)

		results := make([]SeriesWithIndex, 0)
		for perm := range permChan {
			results = append(results, perm)
		}

		expectedCount := 3
		if totalCount != expectedCount {
			t.Errorf("Expected total count %d, got %d", expectedCount, totalCount)
		}

		if len(results) != 3 {
			t.Errorf("Expected 3 results, got %d", len(results))
		}

		// Extract values for comparison
		foundValues := make([]string, 0)
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
		permChan := make(chan SeriesWithIndex, 10)
		totalCount := 0

		go TagSetPermutationStream(labels, permChan, &totalCount)

		results := make([]SeriesWithIndex, 0)
		for perm := range permChan {
			results = append(results, perm)
		}

		expectedCount := 4 // 2 * 2 = 4 combinations
		if totalCount != expectedCount {
			t.Errorf("Expected total count %d, got %d", expectedCount, totalCount)
		}

		if len(results) != 4 {
			t.Errorf("Expected 4 results, got %d", len(results))
		}

		// Verify all combinations exist
		var combinations []string
		for _, result := range results {
			combination := result.Series[0].Value + "," + result.Series[1].Value // label1 + "," + label2
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
		permChan := make(chan SeriesWithIndex, 100) // Larger buffer to handle all combinations
		totalCount := 0

		go TagSetPermutationStream(labels, permChan, &totalCount)

		results := make([]SeriesWithIndex, 0)
		for perm := range permChan {
			results = append(results, perm)
		}

		expectedCount := 6 // 2 * 3 * 1 = 6 combinations
		if totalCount != expectedCount {
			t.Errorf("Expected total count %d, got %d", expectedCount, totalCount)
		}

		if len(results) != 6 {
			t.Errorf("Expected 6 results, got %d", len(results))
		}

		// Verify all combinations exist
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
	// Initialize the fieldGeneratorsPerFile map to prevent nil pointer reference
	s.fieldGeneratorsPerFile = make(map[string]samples.FloatGenerator)

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
					Name: "region",
					Dist: samples.Distribution{
						Type:   "weighted_preset",
						Preset: regionValues,
					},
				},
				{
					Name: "env",
					Dist: samples.Distribution{
						Type:   "weighted_preset",
						Preset: envValues,
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
	}

	// Test with different pick rates to ensure consistency
	for _, pickRate := range []float32{1.0, 0.5} {
		pickRateStr := fmt.Sprintf("%.1f", pickRate)
		t.Run("PickRate_"+pickRateStr, func(t *testing.T) {
			currentTime := time.Now()
			timeSeriesChan := s.generateTimeSeriesForFileConfig(fileConfig, currentTime, pickRate, 0, 0)

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
}

func TestNewCommandDoesNotExposeDatabaseFlag(t *testing.T) {
	cmd := NewCommand()
	if flag := cmd.Flags().Lookup("database"); flag != nil {
		t.Fatalf("expected no database flag, got %q", flag.Name)
	}
}

func TestGenerateTimeSeriesForFileConfigReplicaLabel(t *testing.T) {
	s := &SampleLoader{}
	s.fieldGeneratorsPerFile = make(map[string]samples.FloatGenerator)

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
					Name: "sigma",
					Dist: samples.Distribution{
						Type:   "weighted_preset",
						Preset: labelsValues,
					},
				},
				{
					Name: "alpha",
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
	}

	fileConfigValue := reflect.ValueOf(&fileConfig).Elem()
	tagOrderField := fileConfigValue.FieldByName("TagOrder")
	if !tagOrderField.IsValid() {
		t.Fatalf("FileConfig missing TagOrder field")
	}
	if tagOrderField.Kind() != reflect.Slice {
		t.Fatalf("FileConfig TagOrder field should be a slice")
	}
	tagOrderField.Set(reflect.ValueOf([]int{1, 0}))

	replicaInsertField := fileConfigValue.FieldByName("ReplicaInsertIndex")
	if !replicaInsertField.IsValid() {
		t.Fatalf("FileConfig missing ReplicaInsertIndex field")
	}
	if replicaInsertField.Kind() != reflect.Int {
		t.Fatalf("FileConfig ReplicaInsertIndex field should be int")
	}
	replicaInsertField.SetInt(1)

	currentTime := time.Now()
	timeSeriesChan := s.generateTimeSeriesForFileConfig(fileConfig, currentTime, 1.0, 0, 0)

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
