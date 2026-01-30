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
	TagOrder           []int
	ReplicaInsertIndex int
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
			num_series := 1
			for _, tag := range data.Tags {
				num_series *= tag.Dist.LabelGenerator().NumCandidates()
			}
			log.Printf("Parsing file: %s, num series: %d\n", path, num_series)
			totalSeries += num_series

			tagOrder, replicaInsertIndex, err := computeTagOrderAndReplicaInsertIndex(data.Tags)
			if err != nil {
				return err
			}

			fileConfigs = append(fileConfigs, FileConfig{
				Name:               metricName,
				Config:             data,
				TagOrder:           tagOrder,
				ReplicaInsertIndex: replicaInsertIndex,
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

func computeTagOrderAndReplicaInsertIndex(tags []Tag) ([]int, int, error) {
	order := make([]int, len(tags))
	for i, tag := range tags {
		if tag.Name == "replica" {
			return nil, 0, fmt.Errorf("tag name \"replica\" is reserved for sample_loader")
		}
		order[i] = i
	}

	sort.Slice(order, func(i, j int) bool {
		return tags[order[i]].Name < tags[order[j]].Name
	})

	insertIndex := 0
	for _, idx := range order {
		if tags[idx].Name < "replica" {
			insertIndex++
		} else {
			break
		}
	}

	return order, insertIndex, nil
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
