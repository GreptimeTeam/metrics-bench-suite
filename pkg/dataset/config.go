// Package dataset produces reproducible, bounded-memory historical metrics datasets.
package dataset

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
	"metrics-bench-suite/pkg/samples"
)

var metricName = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)
var labelName = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// Metric describes one validated metric without expanding its series.
type Metric struct {
	Name         string         `json:"name"`
	File         string         `json:"file"`
	SHA256       string         `json:"sha256"`
	Series       int64          `json:"series"`
	Labels       map[string]int `json:"label_cardinality"`
	Distribution string         `json:"value_distribution"`
}

// Inspection is also returned for invalid collections so catalogs retain diagnostics.
type Inspection struct {
	Valid        bool     `json:"valid"`
	ConfigSHA256 string   `json:"config_sha256"`
	BaseSeries   int64    `json:"base_series"`
	Metrics      []Metric `json:"metrics"`
	Errors       []string `json:"errors"`
	configs      []samples.FileConfig
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func jsonDigest(value any) string {
	data, _ := json.Marshal(value)
	return digest(data)
}

// Inspect reads YAML strictly. It never expands a Cartesian product.
func Inspect(root string) (Inspection, error) {
	result := Inspection{Valid: true, Metrics: []Metric{}, Errors: []string{}}
	names := map[string]string{}
	fail := func(path string, err error) { result.Errors = append(result.Errors, fmt.Sprintf("%s: %v", path, err)) }
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			fail(path, fmt.Errorf("symlinks are not supported"))
			return nil
		}
		if ext := filepath.Ext(path); ext != ".yaml" && ext != ".yml" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var config samples.Config
		decoder := yaml.NewDecoder(bytes.NewReader(data))
		decoder.KnownFields(true)
		if err := decoder.Decode(&config); err != nil {
			fail(path, err)
			return nil
		}
		var extra any
		if err := decoder.Decode(&extra); err != io.EOF {
			fail(path, fmt.Errorf("expected one YAML document"))
			return nil
		}
		name := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
		if !metricName.MatchString(name) {
			fail(path, fmt.Errorf("invalid metric name %q", name))
			return nil
		}
		if previous, ok := names[name]; ok {
			fail(path, fmt.Errorf("duplicate metric %q, also in %s", name, previous))
			return nil
		}
		names[name] = path
		counts, series, err := validateConfig(&config)
		if err != nil {
			fail(path, err)
			return nil
		}
		if result.BaseSeries > math.MaxInt64-series {
			fail(path, fmt.Errorf("total cardinality overflow"))
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if relative == "." {
			relative = filepath.Base(path)
		}
		sort.Slice(config.Tags, func(i, j int) bool { return config.Tags[i].Name < config.Tags[j].Name })
		insert := sort.Search(len(config.Tags), func(i int) bool { return config.Tags[i].Name >= "replica" })
		result.Metrics = append(result.Metrics, Metric{Name: name, File: filepath.ToSlash(relative), SHA256: digest(data), Series: series, Labels: counts, Distribution: strings.ToLower(config.Fields[0].Dist.Type)})
		result.configs = append(result.configs, samples.FileConfig{Name: name, Config: config, ReplicaInsertIndex: insert, SeriesCount: int(series)})
		result.BaseSeries += series
		return nil
	})
	if err != nil {
		return result, err
	}
	if len(result.Metrics) == 0 && len(result.Errors) == 0 {
		fail(root, fmt.Errorf("no metric YAML files found"))
	}
	// File paths and bytes, rather than absolute checkout paths, define config identity.
	sort.Slice(result.Metrics, func(i, j int) bool { return result.Metrics[i].Name < result.Metrics[j].Name })
	sort.Slice(result.configs, func(i, j int) bool { return result.configs[i].Name < result.configs[j].Name })
	result.ConfigSHA256 = jsonDigest(result.Metrics)
	result.Valid = len(result.Errors) == 0
	return result, nil
}

func validateConfig(config *samples.Config) (map[string]int, int64, error) {
	labels := map[string]int{}
	count := int64(1)
	for _, tag := range config.Tags {
		if !labelName.MatchString(tag.Name) || tag.Name == "__name__" || tag.Name == "replica" || tag.Name == "churn_id" {
			return nil, 0, fmt.Errorf("invalid or reserved label %q", tag.Name)
		}
		if _, ok := labels[tag.Name]; ok {
			return nil, 0, fmt.Errorf("duplicate label %q", tag.Name)
		}
		if !strings.EqualFold(tag.Type, "string") {
			return nil, 0, fmt.Errorf("label %s must have type STRING", tag.Name)
		}
		d := tag.Dist
		n := 0
		switch strings.ToLower(d.Type) {
		case "constant_string":
			if _, ok := d.Value.(string); !ok {
				return nil, 0, fmt.Errorf("label %s requires a string value", tag.Name)
			}
			n = 1
		case "replica_string":
			if d.Replica == nil || *d.Replica <= 0 || d.ReplicaPrefix == nil {
				return nil, 0, fmt.Errorf("label %s requires positive replica and replica_prefix", tag.Name)
			}
			n = *d.Replica
		case "weighted_preset":
			seen := map[string]bool{}
			weight := 0
			for _, item := range d.Preset {
				if seen[item.Value] {
					return nil, 0, fmt.Errorf("label %s has duplicate candidate %q", tag.Name, item.Value)
				}
				seen[item.Value] = true
				if item.Weight < 0 || item.Weight > math.MaxInt-weight {
					return nil, 0, fmt.Errorf("label %s has invalid weights", tag.Name)
				}
				weight += item.Weight
			}
			if weight <= 0 {
				return nil, 0, fmt.Errorf("label %s requires positive total preset weight", tag.Name)
			}
			n = len(d.Preset)
		default:
			return nil, 0, fmt.Errorf("label %s has unsupported distribution %q", tag.Name, d.Type)
		}
		if n == 0 || count > int64(math.MaxInt)/int64(n) {
			return nil, 0, fmt.Errorf("label %s has empty candidates or cardinality overflow", tag.Name)
		}
		count *= int64(n)
		labels[tag.Name] = n
	}
	if len(config.Fields) != 1 || !strings.EqualFold(config.Fields[0].Type, "float") {
		return nil, 0, fmt.Errorf("expected exactly one FLOAT field")
	}
	if err := validateField(&config.Fields[0].Dist); err != nil {
		return nil, 0, err
	}
	return labels, count, nil
}

func validateField(d *samples.Distribution) error {
	finite := func(p *float64) bool { return p != nil && !math.IsNaN(*p) && !math.IsInf(*p, 0) }
	integral := func(p *float64) bool {
		return finite(p) && *p == math.Trunc(*p) && *p > float64(math.MinInt)/2 && *p < float64(math.MaxInt)/2
	}
	bounds := func() bool { return finite(d.LowerBound) && finite(d.UpperBound) && *d.LowerBound < *d.UpperBound }
	valid := false
	switch strings.ToLower(d.Type) {
	case "mono_inc":
		valid = d.Step != nil && *d.Step > 0 && *d.Step < math.MaxInt/2 && (d.LowerBound == nil || integral(d.LowerBound)) && (d.UpperBound == nil || (integral(d.UpperBound) && integral(d.LowerBound) && *d.UpperBound >= *d.LowerBound))
	case "random_float", "uniform":
		valid = bounds() && !math.IsInf(*d.UpperBound-*d.LowerBound, 0)
	case "random_int":
		valid = bounds() && integral(d.LowerBound) && integral(d.UpperBound)
	case "normal":
		valid = finite(d.Mean) && finite(d.StdDev) && *d.StdDev >= 0
	case "constant_float":
		if integer, ok := d.Value.(int); ok {
			d.Value = float64(integer)
		}
		value, ok := d.Value.(float64)
		valid = ok && !math.IsNaN(value) && !math.IsInf(value, 0)
	case "noisy":
		valid = d.MaxFluctuation != nil && *d.MaxFluctuation >= 0 && *d.MaxFluctuation <= math.MaxInt/2
	case "periodic":
		valid = d.Period != nil && *d.Period > 0 && d.Amplitude != nil && d.Bias != nil
	}
	if !valid {
		return fmt.Errorf("invalid parameters for field distribution %q", d.Type)
	}
	return nil
}
