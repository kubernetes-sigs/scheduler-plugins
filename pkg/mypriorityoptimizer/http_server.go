// http_server.go

package mypriorityoptimizer

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

type HttpResponse struct {
	Status        string         `json:"status"`
	DurationMs    int64          `json:"duration_ms"`
	Error         string         `json:"error,omitempty"`
	Active        bool           `json:"active"`
	Baseline      *SolverScore   `json:"baseline,omitempty"`
	BestName      string         `json:"best_name,omitempty"`
	Attempts      []SolverResult `json:"attempts,omitempty"`
	PendingBefore int            `json:"pending_before"`
}

// -----------------------------------------------------------------------------
// Test hooks
// -----------------------------------------------------------------------------

// Default to the real methods; tests override these to avoid running the full
// scheduling flow and to control outputs.
var (
	getPodsForHTTP = func(pl *SharedState) ([]*v1.Pod, error) {
		return pl.getPods()
	}

	runFlowForHTTP = func(pl *SharedState, ctx context.Context) (*Plan, *SolverScore, string, *SolverResult, []SolverResult, error) {
		return pl.runOptimizationFlow(ctx, nil)
	}
)

// -----------------------------------------------------------------------------
// HTTP server entrypoint
// -----------------------------------------------------------------------------

func (pl *SharedState) startHTTPServer(ctx context.Context, addr string) {
	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", pl.healthzHandler)
	mux.HandleFunc("/active", pl.activeHandler)
	mux.HandleFunc("/solve", pl.solveHandler)

	server := &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	klog.InfoS("HTTP server started", "addr", addr)

	// Shutdown on context cancel.
	go func() {
		<-ctx.Done()
		shCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = server.Shutdown(shCtx)
	}()

	// Intentionally log errors; do not return them (plugin must stay alive).
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		klog.ErrorS(err, "HTTP server exited unexpectedly")
	}
}

// -----------------------------------------------------------------------------
// Handlers
// -----------------------------------------------------------------------------

func (pl *SharedState) healthzHandler(w http.ResponseWriter, r *http.Request) {
	if !pl.PluginReady.Load() {
		http.Error(w, "warming", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (pl *SharedState) activeHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	resp := HttpResponse{
		Active: pl.Active.Load(),
	}
	writeJSON(w, http.StatusOK, resp)
}

func (pl *SharedState) solveHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	start := time.Now()
	resp := HttpResponse{
		Active: pl.Active.Load(),
	}

	// Not ready yet → early exit
	if !pl.PluginReady.Load() {
		resp.Status = "not-ready"
		resp.DurationMs = time.Since(start).Milliseconds()
		writeJSON(w, http.StatusPreconditionFailed, resp)
		return
	}

	// Count pending pods *before* running any solvers.
	pods, _ := getPodsForHTTP(pl)
	resp.PendingBefore = countPendingPods(pods)

	_, baseline, bestName, _, attempts, err := runFlowForHTTP(pl, context.Background())
	resp.Baseline = baseline
	resp.BestName = bestName
	resp.Attempts = attempts
	resp.DurationMs = time.Since(start).Milliseconds()
	if err != nil {
		resp.Error = err.Error()
	}

	switch err {
	case nil:
		resp.Status = "ok"
	case ErrActiveInProgress:
		resp.Status = "busy"
	case ErrNoImprovingSolutionFromAnySolver, ErrNoPendingPodsToSchedule, ErrNoPendingPods:
		resp.Status = "noop"
	default:
		resp.Status = "error"
	}

	writeJSON(w, http.StatusOK, resp)
}

// -----------------------------------------------------------------------------
// Helper
// -----------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
