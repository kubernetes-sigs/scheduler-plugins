// logging_helpers_test.go
package mypriorityoptimizer

import "testing"

// -------------------------
// msg
// --------------------------

func TestMsg_Basic(t *testing.T) {
	got := msg("solver", "started")
	want := "solver: started"
	if got != want {
		t.Fatalf("msg() = %q, want %q", got, want)
	}
}

func TestMsg_EmptyMessenger(t *testing.T) {
	got := msg("", "no owner")
	want := ": no owner"
	if got != want {
		t.Fatalf("msg() with empty messenger = %q, want %q", got, want)
	}
}

func TestMsg_EmptyMessage(t *testing.T) {
	got := msg("scheduler", "")
	want := "scheduler: "
	if got != want {
		t.Fatalf("msg() with empty message = %q, want %q", got, want)
	}
}

func TestMsg_Unicode(t *testing.T) {
	got := msg("øæå", "✔")
	want := "øæå: ✔"
	if got != want {
		t.Fatalf("msg() with unicode = %q, want %q", got, want)
	}
}
