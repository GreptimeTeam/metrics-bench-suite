package samples

// LabelPair is an ordered label name and value pair.
type LabelPair struct {
	Name  string
	Value string
}

// SeriesWithIndex represents a series with its index position in the permutation stream.
type SeriesWithIndex struct {
	Series []LabelPair
	Index  []int
}

// TagSetPermutationStream calls fn for each label permutation (combination of label values).
func TagSetPermutationStream(labels []LabelCandidates, fn func(SeriesWithIndex)) {
	tagSetPermutationStream(labels, func(series SeriesWithIndex) bool {
		fn(series)
		return true
	})
}

func tagSetPermutationStream(labels []LabelCandidates, fn func(SeriesWithIndex) bool) {
	if len(labels) == 0 {
		fn(SeriesWithIndex{
			Series: make([]LabelPair, 0),
			Index:  make([]int, 0),
		})
		return
	}

	currentIndices := make([]int, len(labels))
	end := make([]int, len(labels))
	for i, label := range labels {
		end[i] = len(label.Values)
	}

	for {
		series := make([]LabelPair, 0, len(labels))
		for i, label := range labels {
			series = append(series, LabelPair{
				Name:  label.Name,
				Value: label.Values[currentIndices[i]],
			})
		}
		if !fn(SeriesWithIndex{
			Series: series,
			Index:  append([]int(nil), currentIndices...),
		}) {
			return
		}

		i := 0
		for i < len(currentIndices) {
			currentIndices[i]++
			if currentIndices[i] < end[i] {
				break
			}
			currentIndices[i] = 0
			i++
		}
		if i >= len(currentIndices) {
			break
		}
	}
}
