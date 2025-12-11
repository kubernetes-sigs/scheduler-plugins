package mypriorityoptimizer

import (
	"fmt"
	"time"
)

// getUniqueId generates a unique identifier with the given prefix.
func getUniqueId(prefix string) string {
	return fmt.Sprintf("%s%d", prefix, time.Now().UnixNano())
}

func getTimestampNowUtc() time.Time {
	return time.Now().UTC()
}
