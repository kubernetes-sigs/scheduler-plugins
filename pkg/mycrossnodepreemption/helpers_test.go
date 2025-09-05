package mycrossnodepreemption

import (
	"testing"
)

// Write unit test for comparePlaced
func TestComparePlaced(t *testing.T) {
	a := map[string]int{"1": 1, "2": 2, "3": 3}
	b := map[string]int{"1": 1, "2": 2, "3": 4}
	if comparePlaced(a, b) != -1 {
		t.Errorf("Expected -1, got %d", comparePlaced(a, b))
	}

	c := map[string]int{"1": 1, "2": 2, "3": 3}
	if comparePlaced(a, c) != 0 {
		t.Errorf("Expected 0, got %d", comparePlaced(a, c))
	}
	d := map[string]int{"1": 1, "2": 2, "3": 2}
	if comparePlaced(a, d) != 1 {
		t.Errorf("Expected 1, got %d", comparePlaced(a, d))
	}
}
