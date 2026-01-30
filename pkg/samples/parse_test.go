package samples

import (
	"math"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestWalkAndParseConfigWithMaxFileCountRejectsReplicaLabel(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "test.yaml")
	configBody := []byte(`start: "2025-01-01T00:00:00Z"
end: "2025-01-01T00:01:00Z"
interval: 30
tags:
  - name: replica
    type: string
    dist:
      type: constant_string
      value: foo
fields:
  - name: value
    type: float
    dist:
      type: uniform
      lower_bound: 0
      upper_bound: 1
`)
	if err := os.WriteFile(configPath, configBody, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	_, err := WalkAndParseConfigWithMaxFileCount(dir, math.MaxUint64)
	if err == nil {
		t.Fatalf("expected error for replica label in config")
	}
}

func TestWalkAndParseConfigWithMaxFileCountComputesReplicaInsertIndex(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "test.yaml")
	configBody := []byte(`start: "2025-01-01T00:00:00Z"
end: "2025-01-01T00:01:00Z"
interval: 30
tags:
  - name: sigma
    type: string
    dist:
      type: constant_string
      value: s
  - name: alpha
    type: string
    dist:
      type: constant_string
      value: a
  - name: zeta
    type: string
    dist:
      type: constant_string
      value: z
fields:
  - name: value
    type: float
    dist:
      type: uniform
      lower_bound: 0
      upper_bound: 1
`)
	if err := os.WriteFile(configPath, configBody, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	configs, err := WalkAndParseConfigWithMaxFileCount(dir, math.MaxUint64)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	if len(configs) != 1 {
		t.Fatalf("expected 1 config, got %d", len(configs))
	}

	configValue := reflect.ValueOf(configs[0])
	tagOrderField := configValue.FieldByName("TagOrder")
	if !tagOrderField.IsValid() {
		t.Fatalf("FileConfig missing TagOrder field")
	}
	replicaIndexField := configValue.FieldByName("ReplicaInsertIndex")
	if !replicaIndexField.IsValid() {
		t.Fatalf("FileConfig missing ReplicaInsertIndex field")
	}

	tagOrder, ok := tagOrderField.Interface().([]int)
	if !ok {
		t.Fatalf("TagOrder should be []int")
	}
	expectedOrder := []int{1, 0, 2}
	if !reflect.DeepEqual(tagOrder, expectedOrder) {
		t.Fatalf("expected TagOrder %v, got %v", expectedOrder, tagOrder)
	}

	if replicaIndexField.Int() != 1 {
		t.Fatalf("expected ReplicaInsertIndex 1, got %d", replicaIndexField.Int())
	}
}
