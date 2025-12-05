// args_parsers_test.go

package mypriorityoptimizer

import (
	"testing"
	"time"
)

// -----------------------------------------------------------------------------
// parseGetEnv
// -----------------------------------------------------------------------------

func TestGetEnv(t *testing.T) {
	t.Run("returns existing value", func(t *testing.T) {
		t.Setenv("TEST_KEY", "value")
		got := getenv("TEST_KEY", "default")
		if got != "value" {
			t.Fatalf("getenv() = %q, want %q", got, "value")
		}
	})

	t.Run("returns default when unset", func(t *testing.T) {
		// no Setenv -> env var is unset
		got := getenv("UNSET_KEY", "default")
		if got != "default" {
			t.Fatalf("getenv() = %q, want %q", got, "default")
		}
	})

	t.Run("returns default when set to empty string", func(t *testing.T) {
		t.Setenv("EMPTY_KEY", "")
		got := getenv("EMPTY_KEY", "default")
		if got != "default" {
			t.Fatalf("getenv() with empty value = %q, want %q", got, "default")
		}
	})
}

// -----------------------------------------------------------------------------
// parseBool
// -----------------------------------------------------------------------------

func TestParseBool(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{"true lowercase", "true", true},
		{"TRUE uppercase", "TRUE", true},
		{"false lowercase", "false", false},
		{"FALSE uppercase", "FALSE", false},
		{"numeric 1", "1", true},
		{"numeric 0", "0", false},
		{"invalid -> false", "notabool", false}, // we are okay with returning false on invalid input
		{"empty -> false", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseBool(tt.in)
			if got != tt.want {
				t.Fatalf("parseBool(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// parseInt
// -----------------------------------------------------------------------------

func TestParseInt(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want int
	}{
		{"zero", "0", 0},
		{"positive", "42", 42},
		{"negative", "-7", -7},
		{"invalid -> 0", "notanint", 0},
		{"empty -> 0", "", 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseInt(tt.in)
			if got != tt.want {
				t.Fatalf("parseInt(%q) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// parseFloat
// -----------------------------------------------------------------------------

func TestParseFloat(t *testing.T) {
	tests := []struct {
		name   string
		in     string
		lLimit float64
		uLimit float64
		want   float64
	}{
		{
			name:   "within bounds",
			in:     "0.5",
			lLimit: 0.0,
			uLimit: 1.0,
			want:   0.5,
		},
		{
			name:   "equal to lower bound",
			in:     "0.2",
			lLimit: 0.2,
			uLimit: 1.0,
			want:   0.2,
		},
		{
			name:   "equal to upper bound",
			in:     "1.0",
			lLimit: 0.0,
			uLimit: 1.0,
			want:   1.0,
		},
		{
			name:   "below lower bound is clamped",
			in:     "-1.0",
			lLimit: 0.1,
			uLimit: 2.0,
			want:   0.1,
		},
		{
			name:   "above upper bound is clamped",
			in:     "3.5",
			lLimit: 0.0,
			uLimit: 3.0,
			want:   3.0,
		},
		{
			name:   "invalid parse uses zero",
			in:     "notafloat",
			lLimit: 0.2,
			uLimit: 0.8,
			want:   0.0,
		},
		{
			name:   "invalid parse with non-positive lower bound uses zero",
			in:     "notafloat",
			lLimit: -1.0,
			uLimit: 10.0,
			want:   0.0, // 0.0 is within [-1, 10], so returned as-is
		},
	}

	const floatTolerance = 1e-9

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseFloat(tt.in, tt.lLimit, tt.uLimit)
			diff := got - tt.want
			if diff < 0 {
				diff = -diff
			}
			if diff > floatTolerance {
				t.Fatalf("parseFloat(%q, %f, %f) = %f, want %f",
					tt.in, tt.lLimit, tt.uLimit, got, tt.want)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// parseTime
// -----------------------------------------------------------------------------

func TestParseTime(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want time.Duration
	}{
		{"seconds", "1s", time.Second},
		{"milliseconds", "250ms", 250 * time.Millisecond},
		{"minutes and seconds", "2m3s", 2*time.Minute + 3*time.Second},
		{"zero duration", "0s", 0},
		{"invalid -> 0", "notaduration", 0},
		{"empty -> 0", "", 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseTime(tt.in)
			if got != tt.want {
				t.Fatalf("parseTime(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// parseOptimizeMode
// -----------------------------------------------------------------------------

func TestParseOptimizeMode(t *testing.T) {
	tests := []struct {
		in   string
		want ModeType
	}{
		{"per_pod", ModePerPod},
		{"PER_POD", ModePerPod},
		{"periodic ", ModePeriodic},
		{"interlude", ModeInterlude},
		{"manual", ModeManual},
		{"unknown", ModePeriodic}, // default
	}
	for _, tt := range tests {
		got := parseOptimizeMode(tt.in)
		if got != tt.want {
			t.Fatalf("parseOptimizeMode(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

// -----------------------------------------------------------------------------
// parseOptimizeHookStage
// -----------------------------------------------------------------------------

func TestParseOptimizeHookStage_ManualRespectsEnv(t *testing.T) {
	// Manual mode: both PreEnqueue and PostFilter must be respected (no override).
	withGlobals(ModeManual, StageNone, true, func() {
		if got := parseOptimizeHookStage("preenqueue"); got != StagePreEnqueue {
			t.Fatalf("manual+preenqueue: got %v, want StagePreEnqueue", got)
		}
		if got := parseOptimizeHookStage("postfilter"); got != StagePostFilter {
			t.Fatalf("manual+postfilter: got %v, want StagePostFilter", got)
		}
	})
}

func TestParseOptimizeHookStage_SyncOrNonGlobalHonorsEnv(t *testing.T) {
	// Sync global → respect env
	withGlobals(ModePeriodic, StageNone, true, func() {
		if optimizeAsync() {
			t.Fatalf("test precondition: expected optimizeAsync() false for ModePeriodic + OptimizeSolveSynch=true")
		}
		if got := parseOptimizeHookStage("preenqueue"); got != StagePreEnqueue {
			t.Fatalf("periodic synch + preenqueue input: got %v, want StagePreEnqueue", got)
		}
		if got := parseOptimizeHookStage("postfilter"); got != StagePostFilter {
			t.Fatalf("periodic synch + postfilter input: got %v, want StagePostFilter", got)
		}
	})

	// Non-global (PerPod) → respect env.
	withGlobals(ModePerPod, StageNone, false, func() {
		if got := parseOptimizeHookStage("preenqueue"); got != StagePreEnqueue {
			t.Fatalf("perpod + preenqueue input: got %v, want StagePreEnqueue", got)
		}
		if got := parseOptimizeHookStage("postfilter"); got != StagePostFilter {
			t.Fatalf("perpod + postfilter input: got %v, want StagePostFilter", got)
		}
		if got := parseOptimizeHookStage("unknown"); got != StageNone {
			t.Fatalf("perpod + unknown input: got %v, want StageNone", got)
		}
	})
}
