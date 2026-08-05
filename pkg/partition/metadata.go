// Package partition provides safe parsing and discovery helpers for GreptimeDB
// physical-table partitions.
package partition

import (
	"fmt"
	"sort"
)

// Metadata is one authoritative INFORMATION_SCHEMA.PARTITIONS row.
type Metadata struct {
	Name        string
	Description string
	RegionID    uint64
	Expression  string
	Ordinal     uint64
}

// ConfigTable describes one config-generated logical metric table without
// coupling this package to a particular loader implementation.
type ConfigTable struct {
	Name        string
	LabelValues map[string][]string
	SeriesCount uint64
}

// RegionDistribution is the expected contribution of configured time series
// to one current physical-table region.
type RegionDistribution struct {
	RegionID      uint64
	PartitionName string
	LogicalTables []string
	SeriesCount   uint64
	Unresolved    bool
	Reason        string
}

// MetadataPredicates derives one safe predicate per metadata row. Metadata is
// authoritative; the physical-table DDL is consulted only for a row whose
// description cannot be parsed directly.
func MetadataPredicates(definition PartitionDefinition, metadata []Metadata) []*Predicate {
	predicates := make([]*Predicate, len(metadata))
	uniqueNames, uniqueOrdinals := metadataIdentities(metadata)
	for index, item := range metadata {
		if !uniqueNames[item.Name] || !uniqueOrdinals[item.Ordinal] {
			continue
		}
		if predicate, ok := metadataPredicate(item); ok {
			predicates[index] = &predicate
		}
	}
	for index, item := range metadata {
		if predicates[index] != nil || !uniqueNames[item.Name] || !uniqueOrdinals[item.Ordinal] || item.Ordinal < 1 || item.Ordinal > uint64(len(definition.Partitions)) {
			continue
		}
		columns, err := parseColumns(item.Expression)
		if err != nil || !sameColumns(columns, definition.Columns) {
			continue
		}
		predicate, err := BuildPartitionPredicate(definition.Partitions[item.Ordinal-1])
		if err == nil {
			predicates[index] = &predicate
		}
	}
	return predicates
}

// FindMetadataByColumnValue finds the single partition whose conditions match
// value for column. It deliberately rejects multi-column partitions because a
// single label value cannot prove a unique target in that layout.
func FindMetadataByColumnValue(definition PartitionDefinition, metadata []Metadata, column, value string) (Metadata, Predicate, bool) {
	predicates := MetadataPredicates(definition, metadata)
	var found Metadata
	var predicate Predicate
	matched := false
	for index, item := range metadata {
		if predicates[index] == nil {
			continue
		}
		partition, ok := metadataPartition(&definition, item)
		if !ok || !matchesSingleColumnValue(partition, column, value) {
			continue
		}
		if matched {
			return Metadata{}, Predicate{}, false
		}
		found, predicate, matched = item, *predicates[index], true
	}
	return found, predicate, matched
}

// ConfigRegionDistribution maps every configured partition-label combination
// to the current partition metadata. A nil definition means metadata
// descriptions must be parseable; callers may retry with SHOW CREATE TABLE
// parsed into definition when this returns an error.
func ConfigRegionDistribution(definition *PartitionDefinition, metadata []Metadata, tables []ConfigTable) ([]RegionDistribution, error) {
	columns, err := partitionColumns(metadata)
	if err != nil {
		return nil, err
	}
	byRegion := make(map[uint64]*RegionDistribution)
	for _, table := range tables {
		values := make([][]string, len(columns))
		combinationCount := uint64(1)
		for index, column := range columns {
			values[index] = uniqueStrings(table.LabelValues[column])
			if len(values[index]) == 0 {
				recordUnresolved(byRegion, table, fmt.Sprintf("missing configured values for partition column %q", column))
				combinationCount = 0
				break
			}
			combinationCount *= uint64(len(values[index]))
		}
		if combinationCount == 0 {
			continue
		}
		if table.SeriesCount%combinationCount != 0 {
			recordUnresolved(byRegion, table, fmt.Sprintf("cannot distribute %d series over %d partition combinations", table.SeriesCount, combinationCount))
			continue
		}
		seriesPerCombination := table.SeriesCount / combinationCount
		tableDistribution := make(map[uint64]*RegionDistribution)
		unresolvedReason := ""
		forEachCombination(columns, values, func(labels map[string]string) {
			if unresolvedReason != "" {
				return
			}
			item, ok := ResolveMetadataByValues(definition, metadata, labels)
			if !ok {
				unresolvedReason = fmt.Sprintf("no unique current partition for labels %v", labels)
				return
			}
			distribution := tableDistribution[item.RegionID]
			if distribution == nil {
				distribution = &RegionDistribution{RegionID: item.RegionID, PartitionName: item.Name}
				tableDistribution[item.RegionID] = distribution
			}
			distribution.SeriesCount += seriesPerCombination
			if !containsString(distribution.LogicalTables, table.Name) {
				distribution.LogicalTables = append(distribution.LogicalTables, table.Name)
			}
		})
		if unresolvedReason != "" {
			recordUnresolved(byRegion, table, unresolvedReason)
			continue
		}
		for regionID, distribution := range tableDistribution {
			current := byRegion[regionID]
			if current == nil {
				current = &RegionDistribution{RegionID: distribution.RegionID, PartitionName: distribution.PartitionName}
				byRegion[regionID] = current
			}
			current.SeriesCount += distribution.SeriesCount
			if !containsString(current.LogicalTables, table.Name) {
				current.LogicalTables = append(current.LogicalTables, table.Name)
			}
		}
	}
	result := make([]RegionDistribution, 0, len(byRegion))
	for _, item := range byRegion {
		sort.Strings(item.LogicalTables)
		result = append(result, *item)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].RegionID < result[j].RegionID })
	return result, nil
}

func recordUnresolved(byRegion map[uint64]*RegionDistribution, table ConfigTable, reason string) {
	distribution := byRegion[0]
	if distribution == nil {
		distribution = &RegionDistribution{Unresolved: true, Reason: reason}
		byRegion[0] = distribution
	}
	distribution.SeriesCount += table.SeriesCount
	if !containsString(distribution.LogicalTables, table.Name) {
		distribution.LogicalTables = append(distribution.LogicalTables, table.Name)
	}
}

// ResolveMetadataByValues returns the unique partition matching a complete set
// of partition-column values.
func ResolveMetadataByValues(definition *PartitionDefinition, metadata []Metadata, values map[string]string) (Metadata, bool) {
	var found Metadata
	matched := false
	for _, item := range metadata {
		partition, ok := metadataPartition(definition, item)
		if !ok || !matchesPartitionValues(partition, values) {
			continue
		}
		if matched {
			return Metadata{}, false
		}
		found, matched = item, true
	}
	return found, matched
}

func metadataPartition(definition *PartitionDefinition, item Metadata) (Partition, bool) {
	columns, err := parseColumns(item.Expression)
	if err == nil {
		if partition, err := parsePartition(item.Description, columns); err == nil {
			return partition, true
		}
	}
	if definition == nil || item.Ordinal < 1 || item.Ordinal > uint64(len(definition.Partitions)) || !sameColumns(columns, definition.Columns) {
		return Partition{}, false
	}
	return definition.Partitions[item.Ordinal-1], true
}

func partitionColumns(metadata []Metadata) ([]string, error) {
	if len(metadata) == 0 {
		return nil, fmt.Errorf("physical table has no partition metadata")
	}
	columns, err := parseColumns(metadata[0].Expression)
	if err != nil {
		return nil, err
	}
	for _, item := range metadata[1:] {
		other, err := parseColumns(item.Expression)
		if err != nil || !sameColumns(columns, other) {
			return nil, fmt.Errorf("partition metadata has inconsistent partition columns")
		}
	}
	return columns, nil
}

func forEachCombination(columns []string, values [][]string, emit func(map[string]string)) {
	labels := make(map[string]string, len(columns))
	var visit func(int)
	visit = func(index int) {
		if index == len(columns) {
			copy := make(map[string]string, len(labels))
			for key, value := range labels {
				copy[key] = value
			}
			emit(copy)
			return
		}
		for _, value := range values[index] {
			labels[columns[index]] = value
			visit(index + 1)
		}
	}
	visit(0)
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if _, ok := seen[value]; !ok {
			seen[value] = struct{}{}
			result = append(result, value)
		}
	}
	return result
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func matchesPartitionValues(partition Partition, values map[string]string) bool {
	if len(partition.Conditions) == 0 {
		return false
	}
	for _, condition := range partition.Conditions {
		value, ok := values[condition.Column]
		if !ok || !matchesCondition(value, fmt.Sprint(condition.Value), condition.Operator) {
			return false
		}
	}
	return true
}

func matchesSingleColumnValue(partition Partition, column, value string) bool {
	if len(partition.Conditions) == 0 {
		return false
	}
	for _, condition := range partition.Conditions {
		if condition.Column != column || !matchesCondition(value, fmt.Sprint(condition.Value), condition.Operator) {
			return false
		}
	}
	return true
}

func matchesCondition(value, bound, operator string) bool {
	comparison := 0
	if value < bound {
		comparison = -1
	} else if value > bound {
		comparison = 1
	}
	switch operator {
	case "=":
		return comparison == 0
	case ">":
		return comparison > 0
	case ">=":
		return comparison >= 0
	case "<":
		return comparison < 0
	case "<=":
		return comparison <= 0
	default:
		return false
	}
}

func metadataIdentities(metadata []Metadata) (map[string]bool, map[uint64]bool) {
	nameCounts := make(map[string]int, len(metadata))
	ordinalCounts := make(map[uint64]int, len(metadata))
	for _, item := range metadata {
		nameCounts[item.Name]++
		ordinalCounts[item.Ordinal]++
	}
	names := make(map[string]bool, len(nameCounts))
	for name, count := range nameCounts {
		names[name] = name != "" && count == 1
	}
	ordinals := make(map[uint64]bool, len(ordinalCounts))
	for ordinal, count := range ordinalCounts {
		ordinals[ordinal] = ordinal != 0 && count == 1
	}
	return names, ordinals
}

func metadataPredicate(item Metadata) (Predicate, bool) {
	columns, err := parseColumns(item.Expression)
	if err != nil {
		return Predicate{}, false
	}
	definition, err := parsePartition(item.Description, columns)
	if err != nil {
		return Predicate{}, false
	}
	predicate, err := BuildPartitionPredicate(definition)
	return predicate, err == nil
}

func sameColumns(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
