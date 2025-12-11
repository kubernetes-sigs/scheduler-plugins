// common_helpers_test.go
package mypriorityoptimizer

import (
	"strings"
	"testing"
	"time"
)

// -----------------------------------------------------------------------------
// getUniqueId
// -----------------------------------------------------------------------------

func TestGetUniqueId_PrefixAndNonEmpty(t *testing.T) {
	prefix := "job-"
	id := getUniqueId(prefix)

	if !strings.HasPrefix(id, prefix) {
		t.Fatalf("getUniqueId(%q) = %q, expected prefix %q", prefix, id, prefix)
	}
	if len(id) <= len(prefix) {
		t.Fatalf("getUniqueId(%q) = %q, expected suffix after prefix", prefix, id)
	}
}

func TestGetUniqueId_ProducesDifferentValues(t *testing.T) {
	prefix := "job-"

	id1 := getUniqueId(prefix)
	time.Sleep(time.Nanosecond)
	id2 := getUniqueId(prefix)

	if id1 == id2 {
		t.Fatalf("getUniqueId produced identical values: %q and %q", id1, id2)
	}
}

// -----------------------------------------------------------------------------
// getTimestampNowUtc
// -----------------------------------------------------------------------------

func TestGetTimestampNowUtc_IsUTCAndReasonable(t *testing.T) {
	before := time.Now().UTC()
	ts := getTimestampNowUtc()
	after := time.Now().UTC()

	// Must be in UTC
	if ts.Location() != time.UTC {
		t.Fatalf("getTimestampNowUtc() location = %v, want %v", ts.Location(), time.UTC)
	}

	// Should be within [before-1s, after+1s]
	if ts.Before(before.Add(-1*time.Second)) || ts.After(after.Add(1*time.Second)) {
		t.Fatalf("getTimestampNowUtc() = %v, want close to [%v, %v]", ts, before, after)
	}
}
