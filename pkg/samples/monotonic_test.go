package samples

import (
	"testing"
)

// TestMonoIncMonotonic verifies that the MonoInc distribution
// generates monotonically increasing values, specifically for
// the first 4 samples as required
func TestMonoIncMonotonic(t *testing.T) {
	// Create a MonoInc generator with step of 10, lower bound of 1, and upper bound of 1000
	step := 10
	lowerBound := 1.0
	upperBound := 1000.0
	gen := NewMonoInc(step, &lowerBound, &upperBound)

	// Generate first 4 samples
	values := make([]float64, 4)
	for i := 0; i < 4; i++ {
		values[i] = gen.Next()
	}

	// Check that values are monotonically increasing
	for i := 1; i < len(values); i++ {
		if values[i] <= values[i-1] {
			t.Errorf("Value at index %d (%f) is not greater than value at index %d (%f)",
				i, values[i], i-1, values[i-1])
		}
	}

	// Verify the exact sequence based on the config
	// Config has lower_bound: 1, upper_bound: 1000, step: 10
	// So first 4 values should be: 1, 11, 21, 31
	expected := []float64{1, 11, 21, 31}
	for i, expectedValue := range expected {
		if values[i] != expectedValue {
			t.Errorf("Expected value %f at index %d, got %f", expectedValue, i, values[i])
		}
	}
}

// TestMonoIncWithConfigFile tests the monotonic behavior with the specific config file
func TestMonoIncWithConfigFile(t *testing.T) {
	// Parse the config file
	config, err := parseYAML("../../configs/debug_samples_20/rest_client_request_duration_seconds_bucket.yaml")
	if err != nil {
		t.Skipf("Config file not found or not accessible, skipping test: %v", err)
	}

	// Check if the config has the expected field with mono_inc distribution
	if len(config.Fields) == 0 {
		t.Fatal("Config has no fields")
	}

	field := config.Fields[0]
	if field.Dist.Type != "mono_inc" {
		t.Fatalf("Expected distribution type 'mono_inc', got '%s'", field.Dist.Type)
	}

	if field.Dist.Step == nil {
		t.Fatal("Step value is nil in config")
	}

	// Create the field generator
	gen := field.Dist.FieldGenerator()

	// Generate first 100 samples
	values := make([]float64, 100)
	for i := 0; i < 100; i++ {
		values[i] = gen.Next()
	}

	// Check that values are monotonically increasing
	for i := 1; i < len(values); i++ {
		if values[i] <= values[i-1] {
			t.Errorf("Value at index %d (%f) is not greater than value at index %d (%f)",
				i, values[i], i-1, values[i-1])
		}
		if values[i] > *field.Dist.UpperBound || values[i] < *field.Dist.LowerBound {
			t.Errorf("Value at index %d (%f) is not within bound [%f, %f]",
				i, values[i], *field.Dist.LowerBound, *field.Dist.UpperBound)
		}
	}
	restart := gen.Next()
	if restart >= values[99] {
		t.Errorf("Value at index 100 (%f) should restart",
			values[99])
	}
}
