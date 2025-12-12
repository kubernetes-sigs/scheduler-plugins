// common_helpers_test.go
package mypriorityoptimizer

import (
	"strings"
	"testing"
	"time"
)

// -------------------------
// getUniqueId
// -------------------------

func TestGetUniqueId_UniqueAndPrefix(t *testing.T) {
	old := nowUnixNano
	defer func() { nowUnixNano = old }()

	i := int64(100)
	nowUnixNano = func() int64 { i++; return i }

	id1 := getUniqueId("job-")
	id2 := getUniqueId("job-")

	if id1 == id2 {
		t.Fatalf("expected unique ids")
	}
	if !strings.HasPrefix(id1, "job-") || !strings.HasPrefix(id2, "job-") {
		t.Fatalf("expected prefix")
	}
}

// -------------------------
// getTimestampNowUtc
// -------------------------

func TestGetTimestampNowUtc_IsUTC(t *testing.T) {
	ts := getTimestampNowUtc()
	if ts.Location() != time.UTC {
		t.Fatalf("location=%v want UTC", ts.Location())
	}
}

// -------------------------
// cmpInt
// -------------------------

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

// -------------------------
// cmpLexiByKeys
// -------------------------

func TestCmpLexiByKeys_IntKeys(t *testing.T) {
	keys := []int{2, 1, 0}

	type tc struct {
		name string
		a, b map[int]int
		want int
	}

	tests := []tc{
		{
			name: "most-significant key wins",
			a:    map[int]int{2: 1, 1: 0, 0: 0},
			b:    map[int]int{2: 0, 1: 100, 0: 100},
			want: 1,
		},
		{
			name: "missing keys default to 0",
			a:    map[int]int{2: 1}, // 1,0 -> 0
			b:    map[int]int{},     // all -> 0
			want: 1,
		},
		{
			name: "equal maps",
			a:    map[int]int{2: 1, 1: 2},
			b:    map[int]int{2: 1, 1: 2},
			want: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := cmpLexiByKeys(keys, tt.a, tt.b); got != tt.want {
				t.Fatalf("cmpLexiByKeys(a,b)=%d want %d; a=%v b=%v", got, tt.want, tt.a, tt.b)
			}

			// Invariant: antisymmetry (works for all cases)
			gotAB := cmpLexiByKeys(keys, tt.a, tt.b)
			gotBA := cmpLexiByKeys(keys, tt.b, tt.a)
			if gotAB != -gotBA {
				t.Fatalf("antisymmetry broken: ab=%d ba=%d; a=%v b=%v", gotAB, gotBA, tt.a, tt.b)
			}

			// Invariant: reflexive
			if got := cmpLexiByKeys(keys, tt.a, tt.a); got != 0 {
				t.Fatalf("reflexive broken: cmp(a,a)=%d; a=%v", got, tt.a)
			}
		})
	}
}

// -------------------------
// cmpLexi
// -------------------------

func TestCmpLexi_Invariants(t *testing.T) {
	cases := []map[string]int{
		{"1": 2, "0": 1},
		{"10": 1},
		{"x": 1, "y": 0},
		{},
	}

	for _, a := range cases {
		if got := cmpLexi(a, a); got != 0 {
			t.Fatalf("cmpLexi(a,a)=%d want 0, a=%v", got, a)
		}
		for _, b := range cases {
			ab := cmpLexi(a, b)
			ba := cmpLexi(b, a)
			if ab != -ba {
				t.Fatalf("antisymmetry broken: a=%v b=%v ab=%d ba=%d", a, b, ab, ba)
			}
		}
	}
}
