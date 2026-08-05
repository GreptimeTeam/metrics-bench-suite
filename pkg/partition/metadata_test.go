package partition

import "testing"

func TestParsePartitionDefinitionAcceptsDoubleQuotedColumns(t *testing.T) {
	definition, err := ParsePartitionDefinition(`CREATE TABLE IF NOT EXISTS "greptime_physical_table" (
  "greptime_timestamp" TIMESTAMP(3) NOT NULL,
  "namespace" STRING NULL,
  TIME INDEX ("greptime_timestamp"),
  PRIMARY KEY ("namespace")
)
PARTITION ON COLUMNS ("namespace") (
  namespace < 'app-1',
  namespace >= 'app-1'
)`)
	if err != nil {
		t.Fatal(err)
	}
	if len(definition.Columns) != 1 || definition.Columns[0] != "namespace" || len(definition.Partitions) != 2 {
		t.Fatalf("unexpected partition definition: %#v", definition)
	}
}

func TestFindMetadataByColumnValueUsesMetadataAndFallbackDDL(t *testing.T) {
	definition, err := ParsePartitionDefinition("PARTITION ON COLUMNS (namespace) ((namespace < 'app-1'), (namespace >= 'app-1'))")
	if err != nil {
		t.Fatal(err)
	}
	metadata := []Metadata{
		{Name: "p0", Ordinal: 1, Expression: "namespace", Description: "namespace < 'app-1'", RegionID: 42},
		// This description is intentionally unsupported; the valid ordinal and
		// expression make the parsed DDL the safe fallback.
		{Name: "p1", Ordinal: 2, Expression: "namespace", Description: "unsupported", RegionID: 43},
	}
	item, predicate, ok := FindMetadataByColumnValue(definition, metadata, "namespace", "app-1")
	if !ok || item.Name != "p1" || predicate.SQL != "`namespace` >= ?" {
		t.Fatalf("unexpected resolved partition: %#v, %#v, %v", item, predicate, ok)
	}
}

func TestFindMetadataByColumnValueRejectsAmbiguousMetadata(t *testing.T) {
	definition, err := ParsePartitionDefinition("PARTITION ON COLUMNS (namespace) ((namespace >= 'app-0'))")
	if err != nil {
		t.Fatal(err)
	}
	metadata := []Metadata{
		{Name: "p0", Ordinal: 1, Expression: "namespace", Description: "namespace >= 'app-0'", RegionID: 42},
		{Name: "p1", Ordinal: 1, Expression: "namespace", Description: "namespace >= 'app-0'", RegionID: 43},
	}
	if _, _, ok := FindMetadataByColumnValue(definition, metadata, "namespace", "app-0"); ok {
		t.Fatal("expected ambiguous metadata to be rejected")
	}
}

func TestConfigRegionDistributionUsesAllConfiguredValues(t *testing.T) {
	metadata := []Metadata{
		{Name: "p0", Ordinal: 1, Expression: "namespace", Description: "namespace < 'app-1'", RegionID: 42},
		{Name: "p1", Ordinal: 2, Expression: "namespace", Description: "namespace >= 'app-1'", RegionID: 43},
	}
	tables := []ConfigTable{{
		Name:        "requests_total",
		SeriesCount: 8,
		LabelValues: map[string][]string{"namespace": {"app-0", "app-1"}, "pod": {"pod-0", "pod-1", "pod-2", "pod-3"}},
	}}
	distribution, err := ConfigRegionDistribution(nil, metadata, tables)
	if err != nil {
		t.Fatal(err)
	}
	if len(distribution) != 2 || distribution[0].RegionID != 42 || distribution[0].SeriesCount != 4 || distribution[1].RegionID != 43 || distribution[1].SeriesCount != 4 {
		t.Fatalf("unexpected distribution: %#v", distribution)
	}
}

func TestConfigRegionDistributionRecordsMissingPartitionLabels(t *testing.T) {
	metadata := []Metadata{{Name: "p0", Ordinal: 1, Expression: "namespace", Description: "namespace >= 'app-0'", RegionID: 42}}
	distribution, err := ConfigRegionDistribution(nil, metadata, []ConfigTable{{Name: "without_namespace", SeriesCount: 3, LabelValues: map[string][]string{"pod": {"a", "b", "c"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(distribution) != 1 || !distribution[0].Unresolved || distribution[0].RegionID != 0 || distribution[0].SeriesCount != 3 {
		t.Fatalf("unexpected unresolved distribution: %#v", distribution)
	}
}
