// args_helpers.go

package mycrossnodepreemption

import (
	"os"
	"strconv"
	"time"
)

// getenv retrieves the value of an environment variable or returns a default value.
func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// parseBool parses a boolean string and returns the corresponding bool value.
func parseBool(s string) bool {
	v, _ := strconv.ParseBool(s)
	return v
}

// parseInt parses an integer string and returns the corresponding int value.
func parseInt(s string) int {
	v, _ := strconv.Atoi(s)
	return v
}

// parseTime parses a duration string and returns the corresponding time.Duration.
func parseTime(s string) time.Duration {
	if d, err := time.ParseDuration(s); err == nil {
		return d
	}
	return 0
}
