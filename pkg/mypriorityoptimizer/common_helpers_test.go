// pkg/mypriorityoptimizer/common_helpers_test.go
// common_helpers_test.go
package mypriorityoptimizer

import (
	"strings"
	"testing"
	"time"
)

// -------------------------
// getUniqueId
// --------------------------

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

// -------------------------
// getTimestampNowUtc
// --------------------------

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

// -------------------------
// cmpLexiByKeys
// --------------------------

func TestCmpLexiByKeys_StringKeys(t *testing.T) {
	keys := []string{"p2", "p1", "p0"} // p2 is most significant

	a := map[string]int{"p2": 1, "p1": 0, "p0": 0}
	b := map[string]int{"p2": 0, "p1": 10, "p0": 10}

	// a should win because it has more on the most significant key p2.
	if got := cmpLexiByKeys(keys, a, b); got != 1 {
		t.Fatalf("cmpLexiByKeys(a,b) = %d, want 1 (a better on p2)", got)
	}
	if got := cmpLexiByKeys(keys, b, a); got != -1 {
		t.Fatalf("cmpLexiByKeys(b,a) = %d, want -1 (b worse on p2)", got)
	}

	// Missing keys should default to 0.
	a2 := map[string]int{"p2": 1} // p1, p0 implicitly 0
	b2 := map[string]int{}        // all zero
	if got := cmpLexiByKeys(keys, a2, b2); got != 1 {
		t.Fatalf("cmpLexiByKeys(a2,b2) = %d, want 1 (missing keys default to 0)", got)
	}

	// Equal maps -> 0
	eqA := map[string]int{"p2": 1, "p1": 2}
	eqB := map[string]int{"p2": 1, "p1": 2}
	if got := cmpLexiByKeys(keys, eqA, eqB); got != 0 {
		t.Fatalf("cmpLexiByKeys(equal) = %d, want 0", got)
	}
}

func TestCmpLexiByKeys_IntKeys_Generic(t *testing.T) {
	keys := []int{2, 1, 0} // 2 is most significant

	a := map[int]int{2: 1, 1: 0, 0: 0}
	b := map[int]int{2: 0, 1: 100, 0: 100}

	if got := cmpLexiByKeys(keys, a, b); got != 1 {
		t.Fatalf("cmpLexiByKeys[int](a,b) = %d, want 1 (a better on key 2)", got)
	}
	if got := cmpLexiByKeys(keys, b, a); got != -1 {
		t.Fatalf("cmpLexiByKeys[int](b,a) = %d, want -1 (b worse on key 2)", got)
	}
}

// -------------------------
// cmpLexi
// --------------------------

func TestCmpLexi_HighPriorityWins(t *testing.T) {
	// Basic numeric priority behavior: higher numeric priority first.
	a := map[string]int{"1": 2, "0": 1}
	b := map[string]int{"1": 1, "0": 5}

	if got := cmpLexi(a, b); got != 1 {
		t.Fatalf("cmpLexi(a,b) = %d, want 1 (a better high-prio)", got)
	}
	if got := cmpLexi(b, a); got != -1 {
		t.Fatalf("cmpLexi(b,a) = %d, want -1 (b worse high-prio)", got)
	}

	// Equal maps
	if got := cmpLexi(a, map[string]int{"1": 2, "0": 1}); got != 0 {
		t.Fatalf("cmpLexi(equal) = %d, want 0", got)
	}
}

func TestCmpLexi_NumericVsNumericAndFallback(t *testing.T) {
	// Numeric keys: ensure we really order by numeric value, not lexicographically.
	// 10 should be considered "higher priority" than 2.
	a := map[string]int{"10": 1}           // one pod at prio 10
	b := map[string]int{"2": 100, "10": 0} // many pods at prio 2 but none at 10

	if got := cmpLexi(a, b); got != 1 {
		t.Fatalf("cmpLexi(numeric) = %d, want 1 (a better on prio 10)", got)
	}

	// Non-numeric fallback: keys compared as strings, descending.
	// keys: "y", "x" -> first compare "y".
	x := map[string]int{"x": 1, "y": 0}
	y := map[string]int{"x": 0, "y": 5}

	// At key "y": x has 0, y has 5 -> y wins => cmpLexi(x,y) = -1.
	if got := cmpLexi(x, y); got != -1 {
		t.Fatalf("cmpLexi(non-numeric x,y) = %d, want -1 (y better on \"y\")", got)
	}
	if got := cmpLexi(y, x); got != 1 {
		t.Fatalf("cmpLexi(non-numeric y,x) = %d, want 1 (y better on \"y\")", got)
	}
}

// -------------------------
// cmpInt
// --------------------------

func TestCmpInt(t *testing.T) {
	if got := cmpInt(1, 2); got != 1 {
		t.Fatalf("cmpInt(1,2) = %d, want 1 (improvement)", got)
	}
	if got := cmpInt(3, 2); got != -1 {
		t.Fatalf("cmpInt(3,2) = %d, want -1 (worse)", got)
	}
	if got := cmpInt(2, 2); got != 0 {
		t.Fatalf("cmpInt(2,2) = %d, want 0 (equal)", got)
	}
}
