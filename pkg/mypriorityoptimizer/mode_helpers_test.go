// mode_helpers_test.go
package mypriorityoptimizer

import "testing"

// -------------------------
// Mode Predicates
// -------------------------

func TestModePredicates(t *testing.T) {
	type tc struct {
		name               string
		mode               ModeType
		synch              bool
		wantPerPod         bool
		wantManualBlocking bool
		wantAsync          bool
		wantSyncStr        string
		wantCombined       string
	}

	tests := []tc{
		{
			name:               "per-pod always synch",
			mode:               ModePerPod,
			synch:              false,
			wantPerPod:         true,
			wantManualBlocking: false,
			wantAsync:          false,
			wantSyncStr:        "Synch",
			wantCombined:       "PerPod/Synch",
		},
		{
			name:               "periodic synch",
			mode:               ModePeriodic,
			synch:              true,
			wantPerPod:         false,
			wantManualBlocking: false,
			wantAsync:          false,
			wantSyncStr:        "Synch",
			wantCombined:       "Periodic/Synch",
		},
		{
			name:               "periodic asynch",
			mode:               ModePeriodic,
			synch:              false,
			wantPerPod:         false,
			wantManualBlocking: false,
			wantAsync:          true,
			wantSyncStr:        "Asynch",
			wantCombined:       "Periodic/Asynch",
		},
		{
			name:               "manual blocking is blocking",
			mode:               ModeManualBlocking,
			synch:              true,
			wantPerPod:         false,
			wantManualBlocking: true,
			wantAsync:          false,
			wantSyncStr:        "Synch",
			wantCombined:       "ManualBlocking/Synch",
		},
		{
			name:               "unknown mode keeps old default string",
			mode:               ModeType(999),
			synch:              true,
			wantPerPod:         false,
			wantManualBlocking: false,
			wantAsync:          false,
			wantSyncStr:        "Synch",
			wantCombined:       "Periodic/Synch", // matches ModeType.String() fallback
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withMode(tt.mode, tt.synch, func() {
				if got := isPerPodMode(); got != tt.wantPerPod {
					t.Fatalf("isPerPodMode()=%v want %v", got, tt.wantPerPod)
				}
				if got := isManualBlockingMode(); got != tt.wantManualBlocking {
					t.Fatalf("isManualBlockingMode()=%v want %v", got, tt.wantManualBlocking)
				}
				if got := isAsyncSolving(); got != tt.wantAsync {
					t.Fatalf("isAsyncSolving()=%v want %v", got, tt.wantAsync)
				}
				if got := getSyncAsString(); got != tt.wantSyncStr {
					t.Fatalf("getSyncAsString()=%q want %q", got, tt.wantSyncStr)
				}
				if got := getModeCombinedAsString(); got != tt.wantCombined {
					t.Fatalf("getModeCombinedAsString()=%q want %q", got, tt.wantCombined)
				}
			})
		})
	}
}

// -------------------------
// ModeType_String
// -------------------------

func TestModeType_String(t *testing.T) {
	tests := []struct {
		name string
		in   ModeType
		want string
	}{
		{"PerPod", ModePerPod, "PerPod"},
		{"Periodic", ModePeriodic, "Periodic"},
		{"Interlude", ModeInterlude, "Interlude"},
		{"Manual", ModeManual, "Manual"},
		{"ManualBlocking", ModeManualBlocking, "ManualBlocking"},
		{"UnknownDefaultsToPeriodic", ModeType(999), "Periodic"}, // default branch
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.in.String(); got != tt.want {
				t.Fatalf("ModeType(%d).String() = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
