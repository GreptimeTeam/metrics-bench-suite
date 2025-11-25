package samples

import (
	"testing"
)

// TestMonoIncBounds tests that MonoInc correctly handles upper and lower bounds
func TestMonoIncBounds(t *testing.T) {
	// Test with upper bound that will be reached
	step := 10
	lowerBound := 1.0
	upperBound := 21.0
	gen := NewMonoInc(step, &lowerBound, &upperBound)

	// Generate values, should cycle after reaching upper bound
	expectedSequence := []float64{1, 11, 21, 1, 11} // After 21, it should wrap around to 1
	
	values := make([]float64, 5)
	for i := 0; i < 5; i++ {
		values[i] = gen.Next()
	}

	for i, expectedValue := range expectedSequence {
		if values[i] != expectedValue {
			t.Errorf("Expected value %f at index %d, got %f", expectedValue, i, values[i])
		}
	}
}

// TestMonoIncNoBounds tests that MonoInc works properly without bounds
func TestMonoIncNoBounds(t *testing.T) {
	// Test without bounds
	step := 5
	gen := NewMonoInc(step, nil, nil)

	// Generate first few values, should be 0, 5, 10, 15, ...
	expectedSequence := []float64{0, 5, 10, 15}
	
	values := make([]float64, 4)
	for i := 0; i < 4; i++ {
		values[i] = gen.Next()
	}

	for i, expectedValue := range expectedSequence {
		if values[i] != expectedValue {
			t.Errorf("Expected value %f at index %d, got %f", expectedValue, i, values[i])
		}
	}
}