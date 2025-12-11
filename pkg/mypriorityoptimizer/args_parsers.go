// args_parsers.go
package mypriorityoptimizer

import (
	"os"
	"strconv"
	"strings"
	"time"

	"k8s.io/klog/v2"
)

// -----------------------------------------------------------------------------
// parseGetEnv
// -----------------------------------------------------------------------------

// getenv retrieves the value of an environment variable or returns a default value.
func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// -----------------------------------------------------------------------------
// parseBool
// -----------------------------------------------------------------------------

// parseBool parses a boolean string and returns the corresponding bool value.
func parseBool(s string) bool {
	v, err := strconv.ParseBool(s)
	if err != nil { // we are okay with returning false on invalid input
		klog.ErrorS(err, "parseBool: invalid boolean string; returning false", "input", s)
		return false
	}
	return v
}

// -----------------------------------------------------------------------------
// parseInt
// -----------------------------------------------------------------------------

// parseInt parses an integer string and returns the corresponding int value.
func parseInt(s string) int {
	v, err := strconv.Atoi(s)
	if err != nil { // we are okay with returning 0 on invalid input
		klog.ErrorS(err, "parseInt: invalid integer string; returning 0", "input", s)
		return 0
	}
	return v
}

// -----------------------------------------------------------------------------
// parseFloat
// -----------------------------------------------------------------------------

// parseFloat parses a float string and returns the corresponding float64 value.
func parseFloat(s string, lLimit float64, uLimit float64) float64 {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil { // we are okay with returning 0 on invalid input
		klog.ErrorS(err, "parseFloat: invalid float string; returning 0", "input", s)
		return 0
	}
	if v < lLimit {
		return lLimit
	}
	if v > uLimit {
		return uLimit
	}
	return v
}

// -----------------------------------------------------------------------------
// parseTime
// -----------------------------------------------------------------------------

// parseTime parses a duration string and returns the corresponding time.Duration.
func parseTime(s string) time.Duration {
	d, err := time.ParseDuration(s)
	if err != nil {
		klog.ErrorS(err, "parseTime: invalid duration string; returning 0", "input", s)
		return 0
	}
	return d
}

// -----------------------------------------------------------------------------
// parseOptimizeMode
// -----------------------------------------------------------------------------

// parseOptimizeMode parses an optimization mode string and returns the ModeType.
func parseOptimizeMode(s string) ModeType {
	v := strings.ToLower(strings.TrimSpace(s))
	switch v {
	case "per_pod", "perpod":
		return ModePerPod
	case "periodic":
		return ModePeriodic
	case "interlude":
		return ModeInterlude
	case "manual":
		return ModeManual
	case "manual_blocking", "manualblocking":
		return ModeManualBlocking
	default:
		klog.InfoS("Unknown ENV: OPTIMIZE_MODE value; defaulting to 'periodic'", "value", s)
		return ModePeriodic
	}
}
