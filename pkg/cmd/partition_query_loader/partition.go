package partitionqueryloader

import (
	"regexp"
	"strings"

	sharedpartition "metrics-bench-suite/pkg/partition"
)

// Compatibility aliases keep the command's public parsing API stable while the
// implementation is shared with other loaders.
type PartitionDefinition = sharedpartition.PartitionDefinition
type Partition = sharedpartition.Partition
type PartitionCondition = sharedpartition.PartitionCondition
type Predicate = sharedpartition.Predicate

var ParsePartitionDefinition = sharedpartition.ParsePartitionDefinition
var BuildPartitionPredicate = sharedpartition.BuildPartitionPredicate

var identifierPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func quoteIdentifier(identifier string) string {
	return "`" + strings.ReplaceAll(identifier, "`", "``") + "`"
}
