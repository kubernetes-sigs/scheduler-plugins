// http_server.go
package mypriorityoptimizer

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"k8s.io/klog/v2"
)

// -----------------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------------

func writeHTTPjson(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

var (
	// Keep only this hook so tests can stub the optimisation flow.
	runOptFlow = func(pl *SharedState, ctx context.Context) (*Plan, *PlannerScore, string, *PlannerResult, []PlannerResult, error) {
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

	// Shutdown on context cancel
	go func() {
		<-ctx.Done()
		shCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = server.Shutdown(shCtx)
	}()

	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		klog.ErrorS(err, "HTTP server exited unexpectedly")
	}
}

// -----------------------------------------------------------------------------
// Handlers
// -----------------------------------------------------------------------------

// healthz
func (pl *SharedState) healthzHandler(w http.ResponseWriter, r *http.Request) {
	klog.InfoS("HTTP /healthz requested")
	if !pl.PluginReady.Load() {
		http.Error(w, "warming", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// active
func (pl *SharedState) activeHandler(w http.ResponseWriter, r *http.Request) {
	klog.InfoS("HTTP /active requested")
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	resp := HttpResponse{
		Active: pl.ActivePlanInProgress.Load(),
	}
	writeHTTPjson(w, http.StatusOK, resp)
}

// solve
func (pl *SharedState) solveHandler(w http.ResponseWriter, r *http.Request) {
	klog.InfoS("HTTP /solve requested")
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	start := time.Now()
	resp := HttpResponse{
		Active: pl.ActivePlanInProgress.Load(),
	}

	// Not ready yet -> early exit
	if !pl.PluginReady.Load() {
		resp.Status = "not-ready"
		resp.DurationMs = time.Since(start).Milliseconds()
		writeHTTPjson(w, http.StatusPreconditionFailed, resp)
		return
	}

	// Count pending pods before running any solvers.
	pods, _ := pl.getPods()
	resp.PendingBefore = countPendingPods(pods)

	_, baseline, bestName, _, attempts, err := runOptFlow(pl, context.Background())
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
	case ErrNoImprovingSolutionFromAnySolver, ErrNoPendingPodsScheduled, ErrNoPendingPods:
		resp.Status = "noop"
	default:
		resp.Status = "error"
	}

	writeHTTPjson(w, http.StatusOK, resp)
}
