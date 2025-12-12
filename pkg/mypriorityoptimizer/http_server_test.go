// http_server_test.go
package mypriorityoptimizer

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
)

// -------------------------
// Test Helpers
// -------------------------

func call(t *testing.T, pl *SharedState, h func(http.ResponseWriter, *http.Request), method, path string) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, nil)
	h(rr, req)
	return rr
}

func mustCode(t *testing.T, rr *httptest.ResponseRecorder, want int) {
	t.Helper()
	if rr.Code != want {
		t.Fatalf("status = %d, want %d (body=%q)", rr.Code, want, rr.Body.String())
	}
}

func mustBody(t *testing.T, rr *httptest.ResponseRecorder, want string) {
	t.Helper()
	if got := rr.Body.String(); got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
}

func decodeHTTP(t *testing.T, rr *httptest.ResponseRecorder) HttpResponse {
	t.Helper()
	var resp HttpResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode JSON response: %v (body=%q)", err, rr.Body.String())
	}
	return resp
}

func withRunOptFlow(t *testing.T, fn func(*SharedState, context.Context) (*Plan, *SolverScore, string, *SolverResult, []SolverResult, error), body func()) {
	t.Helper()
	old := runOptFlow
	runOptFlow = fn
	t.Cleanup(func() { runOptFlow = old })
	body()
}

// -------------------------
// /healthz
// -------------------------

func TestHTTP_Healthz(t *testing.T) {
	tests := []struct {
		name      string
		ready     bool
		wantCode  int
		wantBody  string
		checkBody bool
	}{
		{name: "warming", ready: false, wantCode: http.StatusServiceUnavailable},
		{name: "ready", ready: true, wantCode: http.StatusOK, wantBody: "ok", checkBody: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pl := &SharedState{}
			pl.PluginReady.Store(tt.ready)

			rr := call(t, pl, pl.httpHealthzHandler, http.MethodGet, "/healthz")
			mustCode(t, rr, tt.wantCode)
			if tt.checkBody {
				mustBody(t, rr, tt.wantBody)
			}
		})
	}
}

// -------------------------
// /active
// -------------------------

func TestHTTP_Active(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		active     bool
		wantCode   int
		wantJSON   bool
		wantActive bool
	}{
		{name: "method not allowed", method: http.MethodPost, active: true, wantCode: http.StatusMethodNotAllowed},
		{name: "ok", method: http.MethodGet, active: true, wantCode: http.StatusOK, wantJSON: true, wantActive: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pl := &SharedState{}
			pl.ActivePlanInProgress.Store(tt.active)

			rr := call(t, pl, pl.httpActiveHandler, tt.method, "/active")
			mustCode(t, rr, tt.wantCode)

			if tt.wantJSON {
				resp := decodeHTTP(t, rr)
				if resp.Active != tt.wantActive {
					t.Fatalf("Active = %v, want %v", resp.Active, tt.wantActive)
				}
			}
		})
	}
}

// -------------------------
// /solve
// -------------------------

func TestHTTP_Solve_MethodNotAllowed(t *testing.T) {
	pl := &SharedState{}
	rr := call(t, pl, pl.httpSolveHandler, http.MethodGet, "/solve")
	mustCode(t, rr, http.StatusMethodNotAllowed)
}

func TestHTTP_Solve_NotReady(t *testing.T) {
	pl := &SharedState{}
	pl.ActivePlanInProgress.Store(true) // ensure propagated
	pl.PluginReady.Store(false)

	rr := call(t, pl, pl.httpSolveHandler, http.MethodPost, "/solve")
	mustCode(t, rr, http.StatusPreconditionFailed)

	resp := decodeHTTP(t, rr)
	if resp.Status != "not-ready" {
		t.Fatalf("Status = %q, want %q", resp.Status, "not-ready")
	}
	if !resp.Active {
		t.Fatalf("Active = %v, want true", resp.Active)
	}
	// DurationMs can be 0 on fast machines, so only sanity check it exists.
	if resp.DurationMs < 0 {
		t.Fatalf("DurationMs = %d, want >= 0", resp.DurationMs)
	}
}

func TestHTTP_Solve_Ready_StatusVariants(t *testing.T) {
	// Two pending + one running => PendingBefore should be 2.
	p1 := makePod("ns", "p1", "u1", "", "", "", 0)
	p2 := makePod("ns", "p2", "u2", "", "", "", 0)
	p3 := makePod("ns", "p3", "u3", "n1", "", "", 0)

	store := map[string]map[string]*v1.Pod{
		"ns": {"p1": p1, "p2": p2, "p3": p3},
	}
	fpl := &fakePodLister{store: store}

	attempts := []SolverResult{
		{Name: "solverA", Status: "FEASIBLE"},
		{Name: "solverB", Status: "OPTIMAL"},
	}

	tests := []struct {
		name       string
		err        error
		wantStatus string
		wantErrSet bool
	}{
		{name: "ok", err: nil, wantStatus: "ok", wantErrSet: false},
		{name: "busy", err: ErrActiveInProgress, wantStatus: "busy", wantErrSet: true},
		{name: "noop", err: ErrNoPendingPods, wantStatus: "noop", wantErrSet: true},
		{name: "error", err: fmt.Errorf("boom"), wantStatus: "error", wantErrSet: true},
	}

	withPodLister(fpl, func() {
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				pl := &SharedState{}
				pl.PluginReady.Store(true)
				pl.ActivePlanInProgress.Store(true)

				withRunOptFlow(t, func(*SharedState, context.Context) (*Plan, *SolverScore, string, *SolverResult, []SolverResult, error) {
					return nil, &SolverScore{Evicted: 1}, "solverB", nil, attempts, tt.err
				}, func() {
					rr := call(t, pl, pl.httpSolveHandler, http.MethodPost, "/solve")
					mustCode(t, rr, http.StatusOK)

					resp := decodeHTTP(t, rr)

					if resp.Status != tt.wantStatus {
						t.Fatalf("Status = %q, want %q", resp.Status, tt.wantStatus)
					}
					if !resp.Active {
						t.Fatalf("Active = false, want true")
					}
					if resp.PendingBefore != 2 {
						t.Fatalf("PendingBefore = %d, want 2", resp.PendingBefore)
					}
					if tt.wantErrSet && resp.Error == "" {
						t.Fatalf("expected Error to be populated for err=%v", tt.err)
					}
					if !tt.wantErrSet && resp.Error != "" {
						t.Fatalf("expected Error empty when err=nil, got %q", resp.Error)
					}
				})
			})
		}
	})
}

// -------------------------
// startHttpServer
// -------------------------

func TestStartHttpServer_ShutsDownOnContextCancel(t *testing.T) {
	pl := &SharedState{}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		pl.startHttpServer(ctx, "127.0.0.1:0") // OS picks port
		close(done)
	}()

	// Give ListenAndServe a moment to start.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
		// ok
	case <-time.After(2 * time.Second):
		t.Fatalf("startHttpServer did not shut down after context cancel")
	}
}

func TestStartHttpServer_ListenAndServeError_Returns(t *testing.T) {
	pl := &SharedState{}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // ensure shutdown goroutine doesn't leak

	// Invalid port -> ListenAndServe returns immediately with an error != http.ErrServerClosed.
	pl.startHttpServer(ctx, "127.0.0.1:-1")
}

// -------------------------
// writeHttpJson
// -------------------------

func TestWriteHttpJson_SetsHeaderStatusAndEncodesBody(t *testing.T) {
	rr := httptest.NewRecorder()

	type payload struct {
		A string `json:"a"`
		N int    `json:"n"`
	}
	want := payload{A: "x", N: 7}

	writeHttpJson(rr, http.StatusTeapot, want)

	if rr.Code != http.StatusTeapot {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusTeapot)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want %q", ct, "application/json")
	}

	var got payload
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("response body is not valid JSON: %v (body=%q)", err, rr.Body.String())
	}
	if got != want {
		t.Fatalf("decoded payload = %#v, want %#v", got, want)
	}
}
