package mypriorityoptimizer

import (
	"fmt"
	"time"
)

// -----------------------------------------------------------------------------
// getUniqueId
// -----------------------------------------------------------------------------

// getUniqueId generates a unique identifier with the given prefix.
func getUniqueId(prefix string) string {
	return fmt.Sprintf("%s%d", prefix, time.Now().UnixNano())
}

// -----------------------------------------------------------------------------
// getTimestampNowUtc
// -----------------------------------------------------------------------------

// getTimestampNowUtc returns the current timestamp in UTC.
func getTimestampNowUtc() time.Time {
	return time.Now().UTC()
}
