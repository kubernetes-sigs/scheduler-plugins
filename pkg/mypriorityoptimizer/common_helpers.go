package mypriorityoptimizer

import (
	"fmt"
	"sort"
	"strconv"
	"time"
)

// -------------------------
// getUniqueId
// --------------------------

// getUniqueId generates a unique identifier with the given prefix.
func getUniqueId(prefix string) string {
	return fmt.Sprintf("%s%d", prefix, time.Now().UnixNano())
}

// -------------------------
// getTimestampNowUtc
// --------------------------

// getTimestampNowUtc returns the current timestamp in UTC.
func getTimestampNowUtc() time.Time {
	return time.Now().UTC()
}

// -------------------------
// cmpInt
// --------------------------

// cmpInt returns +1 if suggested<baseline (improvement because smaller is better),
// -1 if suggested>baseline (worse), 0 if equal.
func cmpInt(suggested, baseline int) int {
	switch {
	case suggested < baseline:
		return 1
	case suggested > baseline:
		return -1
	default:
		return 0
	}
}

// -------------------------
// cmpLexiByKeys
// --------------------------

// cmpLexiByKeys compares two score maps lexicographically along the given key
// order. The first key in `keys` is the most significant, the last the least.
// Returns 1 if a>b, -1 if a<b, 0 if equal.
func cmpLexiByKeys[K comparable](keys []K, a, b map[K]int) int {
	for _, k := range keys {
		av := a[k]
		bv := b[k]
		if av == bv {
			continue
		}
		if av > bv {
			return 1
		}
		return -1
	}
	return 0
}

// -------------------------
// cmpLexi
// --------------------------

// cmpLexi compares two maps[string]int whose keys are stringified integers.
// It orders keys by descending numeric value (e.g. "10" > "5" > "1") and
// compares the counts lexicographically along that order.
func cmpLexi(a, b map[string]int) int {
	// Collect union of keys from both maps.
	keySet := make(map[string]struct{}, len(a)+len(b))
	for k := range a {
		keySet[k] = struct{}{}
	}
	for k := range b {
		keySet[k] = struct{}{}
	}

	keys := make([]string, 0, len(keySet))
	for k := range keySet {
		keys = append(keys, k)
	}

	// Sort by descending numeric value when possible; fall back to
	// descending string order if parse fails.
	sort.Slice(keys, func(i, j int) bool {
		ki, kj := keys[i], keys[j]

		iv, ierr := strconv.Atoi(ki)
		jv, jerr := strconv.Atoi(kj)

		if ierr == nil && jerr == nil {
			// Priorities: higher integer first.
			return iv > jv
		}
		// Fallback: simple string comparison (also descending).
		return ki > kj
	})

	return cmpLexiByKeys(keys, a, b)
}
