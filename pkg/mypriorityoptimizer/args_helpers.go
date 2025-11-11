// args_helpers.go

package mypriorityoptimizer

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

// parseFloat parses a float string and returns the corresponding float64 value.
func parseFloat(s string, lLimit float64, uLimit float64) float64 {
	v, _ := strconv.ParseFloat(s, 64)
	if v < lLimit {
		return lLimit
	}
	if v > uLimit {
		return uLimit
	}
	return v
}

// parseTime parses a duration string and returns the corresponding time.Duration.
func parseTime(s string) time.Duration {
	if d, err := time.ParseDuration(s); err == nil {
		return d
	}
	return 0
}
