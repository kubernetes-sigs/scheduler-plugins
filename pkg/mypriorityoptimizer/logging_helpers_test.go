// logging_helpers_test.go
package mypriorityoptimizer

import "testing"

func TestMsg(t *testing.T) {
	tests := []struct {
		name      string
		messenger string
		message   string
		want      string
	}{
		{"basic", "solver", "started", "solver: started"},
		{"empty messenger", "", "no owner", ": no owner"},
		{"empty message", "scheduler", "", "scheduler: "},
		{"unicode", "øæå", "✔", "øæå: ✔"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := msg(tt.messenger, tt.message); got != tt.want {
				t.Fatalf("msg(%q,%q) = %q, want %q", tt.messenger, tt.message, got, tt.want)
			}
		})
	}
}
