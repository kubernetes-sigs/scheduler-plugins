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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// -----------------------------------------------------------------------------
// /healthz endpoint
// -----------------------------------------------------------------------------

func TestHealthzHandler_WarmingAndReady(t *testing.T) {
	pl := &SharedState{}

	// warming
	{
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)

		pl.healthzHandler(rr, req)

		if rr.Code != http.StatusServiceUnavailable {
			t.Fatalf("warming: status = %d, want %d", rr.Code, http.StatusServiceUnavailable)
		}
	}

	// ready
	{
		pl.PluginReady.Store(true)

		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)

		pl.healthzHandler(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("ready: status = %d, want %d", rr.Code, http.StatusOK)
		}
		if body := rr.Body.String(); body != "ok" {
			t.Fatalf("ready: body = %q, want %q", body, "ok")
		}
	}
}

// -----------------------------------------------------------------------------
// /active endpoint
// -----------------------------------------------------------------------------

func TestActiveHandler_MethodNotAllowedAndOK(t *testing.T) {
	pl := &SharedState{}
	pl.Active.Store(true)

	// method not allowed
	{
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/active", nil)

		pl.activeHandler(rr, req)

		if rr.Code != http.StatusMethodNotAllowed {
			t.Fatalf("POST /active status = %d, want %d", rr.Code, http.StatusMethodNotAllowed)
		}
	}

	// happy path
	{
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/active", nil)

		pl.activeHandler(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("GET /active status = %d, want %d", rr.Code, http.StatusOK)
		}
		resp := decodeHTTPResponse(t, rr)
		if !resp.Active {
			t.Fatalf("expected Active=true in response")
		}
	}
}

// -----------------------------------------------------------------------------
// /solve endpoint
// -----------------------------------------------------------------------------

func TestSolveHandler_MethodNotAllowed(t *testing.T) {
	pl := &SharedState{}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/solve", nil)

	pl.solveHandler(rr, req)

	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /solve status = %d, want %d", rr.Code, http.StatusMethodNotAllowed)
	}
}

func TestSolveHandler_NotReady(t *testing.T) {
	pl := &SharedState{}
	pl.Active.Store(true) // just to see it propagated

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/solve", nil)

	pl.solveHandler(rr, req)

	if rr.Code != http.StatusPreconditionFailed {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusPreconditionFailed)
	}
	resp := decodeHTTPResponse(t, rr)
	if resp.Status != "not-ready" {
		t.Fatalf("Status = %q, want %q", resp.Status, "not-ready")
	}
	if !resp.Active {
		t.Fatalf("Active = %v, want true", resp.Active)
	}
}

func TestSolveHandler_Ready_StatusVariants(t *testing.T) {
	// Two pending pods + one running; we expect PendingBefore == 2.
	pods := []*v1.Pod{
		{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "ns",
				Name:      "p1",
				UID:       "u1",
			},
			Spec: v1.PodSpec{},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "ns",
				Name:      "p2",
				UID:       "u2",
			},
			Spec: v1.PodSpec{},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "ns",
				Name:      "p3",
				UID:       "u3",
			},
			Spec: v1.PodSpec{
				NodeName: "n1",
			},
		},
	}

	type tc struct {
		name       string
		err        error
		wantStatus string
	}

	cases := []tc{
		{name: "ok", err: nil, wantStatus: "ok"},
		{name: "busy", err: ErrActiveInProgress, wantStatus: "busy"},
		{name: "noop", err: ErrNoPendingPods, wantStatus: "noop"},
		{name: "error", err: fmt.Errorf("boom"), wantStatus: "error"},
	}

	attempts := []SolverResult{
		{Name: "solverA", Status: "FEASIBLE"},
		{Name: "solverB", Status: "OPTIMAL"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pl := &SharedState{}
			pl.PluginReady.Store(true)
			pl.Active.Store(true)

			// Override hooks for this subtest.
			oldGet := getPodsForHTTP
			oldRun := runFlowForHTTP
			getPodsForHTTP = func(*SharedState) ([]*v1.Pod, error) {
				return pods, nil
			}
			runFlowForHTTP = func(*SharedState, context.Context) (*Plan, *SolverScore, string, *SolverResult, []SolverResult, error) {
				baseline := &SolverScore{Evicted: 1}
				return nil, baseline, "solverB", nil, attempts, c.err
			}
			defer func() {
				getPodsForHTTP = oldGet
				runFlowForHTTP = oldRun
			}()

			rr := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/solve", nil)

			pl.solveHandler(rr, req)

			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d", rr.Code, http.StatusOK)
			}
			resp := decodeHTTPResponse(t, rr)

			if resp.Status != c.wantStatus {
				t.Fatalf("Status = %q, want %q", resp.Status, c.wantStatus)
			}
			if !resp.Active {
				t.Fatalf("Active = false, want true")
			}
			if resp.PendingBefore != 2 {
				t.Fatalf("PendingBefore = %d, want 2", resp.PendingBefore)
			}

			if c.err != nil && resp.Error == "" {
				t.Fatalf("expected Error to be populated for err=%v", c.err)
			}
			if c.err == nil && resp.Error != "" {
				t.Fatalf("expected Error empty when err=nil, got %q", resp.Error)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// startHTTPServer
// -----------------------------------------------------------------------------

func TestStartHTTPServer_ShutsDownOnContextCancel(t *testing.T) {
	pl := &SharedState{}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		// Using :0 lets OS pick a free port
		pl.startHTTPServer(ctx, "127.0.0.1:0")
		close(done)
	}()

	// Give the server a moment to start.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
		// ok
	case <-time.After(2 * time.Second):
		t.Fatalf("startHTTPServer did not shut down after context cancel")
	}
}

// -----------------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------------

func decodeHTTPResponse(t *testing.T, rr *httptest.ResponseRecorder) HttpResponse {
	t.Helper()
	var resp HttpResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode JSON response: %v", err)
	}
	return resp
}
