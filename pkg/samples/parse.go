package samples

import (
	"fmt"
	"io/fs"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"

	"gopkg.in/yaml.v3"
)

// FileConfig represents a parsed YAML configuration file
type FileConfig struct {
	Name               string
	Config             Config
	ReplicaInsertIndex int
	SeriesCount        int
	ChurnIndices       []int
	// FieldGenerators caches field value generators per series key (and churn epoch when applicable).
	// Populated at runtime; not serialized from YAML.
	FieldGenerators map[string]FloatGenerator
}

// GetOrCreateFieldGenerator returns a field generator for the given indices, creating one from dist if needed.
func (f *FileConfig) GetOrCreateFieldGenerator(indices []int) FloatGenerator {
	key := convertIndexToKey(indices)
	if f.FieldGenerators == nil {
		f.FieldGenerators = make(map[string]FloatGenerator)
	}
	if gen, exists := f.FieldGenerators[key]; exists {
		return gen
	}
	gen := f.Config.Fields[0].Dist.FieldGenerator()
	f.FieldGenerators[key] = gen
	return gen
}

// convertIndexToKey converts a label index array to a string key for map indexing
func convertIndexToKey(indices []int) string {
	key := ""
	for i, idx := range indices {
		if i > 0 {
			key += ","
		}
		key += fmt.Sprintf("%d", idx)
	}
	return key
}
func getFileNameWithoutExt(path string) string {
	base := filepath.Base(path)
	ext := filepath.Ext(base)
	return base[:len(base)-len(ext)]
}

// WalkAndParseConfigWithMaxFileCount parses up to tablePickCount YAML files under path.
func WalkAndParseConfigWithMaxFileCount(path string, tablePickCount uint64) ([]FileConfig, error) {
	var fileConfigs []FileConfig

	var totalSeries = 0
	err := filepath.WalkDir(path, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && (filepath.Ext(path) == ".yaml" || filepath.Ext(path) == ".yml") {
			data, err := parseYAML(path)
			if err != nil {
				log.Printf("Error parsing YAML file %s: %v\n", path, err)
				return nil
			}

			metricName := getFileNameWithoutExt(path)
			numSeries := computeSeriesCount(data.Tags)
			log.Printf("Parsing file: %s, num series: %d\n", path, numSeries)
			totalSeries += numSeries

			replicaInsertIndex, err := sortTagsAndComputeReplicaInsertIndex(data.Tags)
			if err != nil {
				return err
			}

			fileConfigs = append(fileConfigs, FileConfig{
				Name:               metricName,
				Config:             data,
				ReplicaInsertIndex: replicaInsertIndex,
				SeriesCount:        numSeries,
			})
			if uint64(len(fileConfigs)) > tablePickCount {
				log.Printf("Warning: More than %d YAML files found. Only the first %d will be used.\n", tablePickCount, tablePickCount)
				return fs.SkipAll
			}
		}
		return nil
	})

	log.Printf("Total series: %d\n", totalSeries)

	if err != nil {
		return nil, err
	}

	return fileConfigs, nil
}

func computeSeriesCount(tags []Tag) int {
	seriesCount := 1
	for _, tag := range tags {
		seriesCount *= tag.Dist.LabelGenerator().NumCandidates()
	}
	return seriesCount
}

// AssignChurnIndices precomputes which series indices should churn for each file config.
// Indices are deterministic: they are assigned based on per-file series counts, sorted by
// file name for remainder distribution, and each file gets the lowest indices [0..N-1].
func AssignChurnIndices(fileConfigs []FileConfig, churnRate float64) {
	if churnRate <= 0 || len(fileConfigs) == 0 {
		for i := range fileConfigs {
			fileConfigs[i].ChurnIndices = nil
		}
		return
	}

	totalSeries := 0
	for i := range fileConfigs {
		if fileConfigs[i].SeriesCount == 0 {
			fileConfigs[i].SeriesCount = computeSeriesCount(fileConfigs[i].Config.Tags)
		}
		totalSeries += fileConfigs[i].SeriesCount
	}
	if totalSeries == 0 {
		return
	}

	target := int(math.Round(churnRate * float64(totalSeries)))
	if target < 0 {
		target = 0
	}
	if target > totalSeries {
		target = totalSeries
	}

	type fileRef struct {
		index int
		name  string
	}
	ordered := make([]fileRef, 0, len(fileConfigs))
	for i := range fileConfigs {
		ordered = append(ordered, fileRef{index: i, name: fileConfigs[i].Name})
	}
	sort.Slice(ordered, func(i, j int) bool {
		return ordered[i].name < ordered[j].name
	})

	remaining := target
	for _, ref := range ordered {
		if remaining == 0 {
			break
		}
		share := int(math.Floor(float64(fileConfigs[ref.index].SeriesCount) / float64(totalSeries) * float64(target)))
		if share > remaining {
			share = remaining
		}
		fileConfigs[ref.index].ChurnIndices = buildChurnIndices(share)
		remaining -= share
	}

	for _, ref := range ordered {
		if remaining == 0 {
			break
		}
		fileConfigs[ref.index].ChurnIndices = append(fileConfigs[ref.index].ChurnIndices, len(fileConfigs[ref.index].ChurnIndices))
		remaining--
	}
}

func buildChurnIndices(count int) []int {
	if count <= 0 {
		return nil
	}
	indices := make([]int, count)
	for i := 0; i < count; i++ {
		indices[i] = i
	}
	return indices
}

func sortTagsAndComputeReplicaInsertIndex(tags []Tag) (int, error) {
	for _, tag := range tags {
		if tag.Name == "replica" {
			return 0, fmt.Errorf("tag name \"replica\" is reserved for sample_loader")
		}
	}

	sort.Slice(tags, func(i, j int) bool {
		return tags[i].Name < tags[j].Name
	})

	insertIndex := 0
	for _, tag := range tags {
		if tag.Name < "replica" {
			insertIndex++
		} else {
			break
		}
	}

	return insertIndex, nil
}

// WalkAndParseConfig walks a directory and parses all YAML files, returning a list of FileConfig
func WalkAndParseConfig(path string) ([]FileConfig, error) {
	return WalkAndParseConfigWithMaxFileCount(path, math.MaxUint64)
}

// parseYAML parses a YAML file and returns a Config
func parseYAML(path string) (Config, error) {
	var config Config
	yamlFile, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}

	err = yaml.Unmarshal(yamlFile, &config)
	if err != nil {
		return Config{}, err
	}

	return config, nil
}
