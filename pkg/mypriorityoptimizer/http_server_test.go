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

// small helper
func decodeHTTPResponse(t *testing.T, rr *httptest.ResponseRecorder) HttpResponse {
	t.Helper()
	var resp HttpResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode JSON response: %v", err)
	}
	return resp
}

// -----------------------------------------------------------------------------
// writeJSON
// -----------------------------------------------------------------------------

func TestWriteJSON_SetsStatusAndContentType(t *testing.T) {
	rr := httptest.NewRecorder()
	payload := map[string]string{"foo": "bar"}

	writeJSON(rr, http.StatusTeapot, payload)

	if rr.Code != http.StatusTeapot {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusTeapot)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	var got map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("invalid JSON body: %v", err)
	}
	if got["foo"] != "bar" {
		t.Fatalf("body[foo] = %q, want %q", got["foo"], "bar")
	}
}

// -----------------------------------------------------------------------------
// /healthz
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
		if body := rr.Body.String(); body != "ok" { // <- changed
			t.Fatalf("ready: body = %q, want %q", body, "ok")
		}
	}
}

// -----------------------------------------------------------------------------
// /active
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
// /solve – method + not-ready
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
	if resp.Active != true {
		t.Fatalf("Active = %v, want true", resp.Active)
	}
	if resp.DurationMs < 0 {
		t.Fatalf("DurationMs = %d, want >= 0", resp.DurationMs)
	}
}

// -----------------------------------------------------------------------------
// /solve – ready path (ok / busy / noop / error)
// -----------------------------------------------------------------------------

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
				attempts := []SolverResult{
					{Name: "solverA", Status: "FEASIBLE"},
					{Name: "solverB", Status: "OPTIMAL"},
				}
				// 4th return is a *SolverResult; we can just return nil there for HTTP purposes.
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
			if resp.DurationMs < 0 {
				t.Fatalf("DurationMs = %d, want >= 0", resp.DurationMs)
			}
			if len(resp.Attempts) != 2 {
				t.Fatalf("len(Attempts) = %d, want 2", len(resp.Attempts))
			}
			if resp.BestName != "solverB" {
				t.Fatalf("BestName = %q, want %q", resp.BestName, "solverB")
			}
			if resp.Baseline == nil || resp.Baseline.Evicted != 1 {
				t.Fatalf("Baseline = %#v, want Evicted=1", resp.Baseline)
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
// startHTTPServer – lifecycle
// -----------------------------------------------------------------------------

func TestStartHTTPServer_ShutsDownOnContextCancel(t *testing.T) {
	pl := &SharedState{}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		// Using :0 lets the OS pick a free port; we don't need to know it.
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
