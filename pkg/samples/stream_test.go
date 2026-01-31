package samples

import (
	"reflect"
	"testing"
)

func TestTagSetPermutationStreamEmptyLabels(t *testing.T) {
	var results []SeriesWithIndex
	TagSetPermutationStream(nil, func(swi SeriesWithIndex) {
		results = append(results, swi)
	})

	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}

	if len(results[0].Series) != 0 {
		t.Fatalf("expected empty series, got %v", results[0].Series)
	}

	if len(results[0].Index) != 0 {
		t.Fatalf("expected empty index, got %v", results[0].Index)
	}
}

func TestTagSetPermutationStreamOrderAndIndex(t *testing.T) {
	labels := []LabelCandidates{
		{
			Name:   "label1",
			Values: []string{"a", "b"},
		},
		{
			Name:   "label2",
			Values: []string{"x", "y"},
		},
	}

	var results []SeriesWithIndex
	TagSetPermutationStream(labels, func(swi SeriesWithIndex) {
		results = append(results, swi)
	})

	expectedSeries := [][]LabelPair{
		{{Name: "label1", Value: "a"}, {Name: "label2", Value: "x"}},
		{{Name: "label1", Value: "b"}, {Name: "label2", Value: "x"}},
		{{Name: "label1", Value: "a"}, {Name: "label2", Value: "y"}},
		{{Name: "label1", Value: "b"}, {Name: "label2", Value: "y"}},
	}
	expectedIndexes := [][]int{
		{0, 0},
		{1, 0},
		{0, 1},
		{1, 1},
	}

	if len(results) != len(expectedSeries) {
		t.Fatalf("expected %d results, got %d", len(expectedSeries), len(results))
	}

	for i, result := range results {
		if !reflect.DeepEqual(result.Series, expectedSeries[i]) {
			t.Fatalf("result %d: expected series %v, got %v", i, expectedSeries[i], result.Series)
		}
		if !reflect.DeepEqual(result.Index, expectedIndexes[i]) {
			t.Fatalf("result %d: expected index %v, got %v", i, expectedIndexes[i], result.Index)
		}
	}
}

func TestTagSetPermutationStreamCopiesSlices(t *testing.T) {
	labels := []LabelCandidates{
		{
			Name:   "label1",
			Values: []string{"a", "b"},
		},
		{
			Name:   "label2",
			Values: []string{"x"},
		},
	}

	var results []SeriesWithIndex
	TagSetPermutationStream(labels, func(swi SeriesWithIndex) {
		results = append(results, swi)
	})

	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}

	results[0].Index[0] = 9
	if results[1].Index[0] != 1 {
		t.Fatalf("expected index slice to be independent, got %v", results[1].Index)
	}

	results[0].Series[0].Value = "changed"
	if results[1].Series[0].Value != "b" {
		t.Fatalf("expected series slice to be independent, got %v", results[1].Series)
	}
}
