// http.go

package mycrossnodepreemption

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"k8s.io/klog/v2"
)

type HttpResponse struct {
	Status        string        `json:"status"`
	DurationMs    int64         `json:"duration_ms"`
	Error         string        `json:"error,omitempty"`
	Active        bool          `json:"active"`
	BestSolver    *SolverResult `json:"best_solver,omitempty"`
	PendingBefore int           `json:"pending_before"`
}

func (pl *MyCrossNodePreemption) startHTTPServer(ctx context.Context, addr string) {
	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if !pl.CachesWarm.Load() {
			http.Error(w, "warming", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	mux.HandleFunc("/active", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		resp := HttpResponse{
			Active: pl.Active.Load(),
		}
		writeJSON(w, http.StatusOK, resp)
	})

	mux.HandleFunc("/optimize", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		start := time.Now()
		resp := HttpResponse{
			Active: pl.Active.Load(),
		}
		if !pl.CachesWarm.Load() {
			resp.Status = "not-ready"
			resp.DurationMs = time.Since(start).Milliseconds()
			writeJSON(w, http.StatusPreconditionFailed, resp)
			return
		}

		pods, _ := pl.getPods()
		resp.PendingBefore = countPendingPods(pods)

		_, bestSolver, err := pl.runFlow(context.Background(), nil)
		if bestSolver != nil {
			resp.BestSolver = bestSolver
		}
		resp.DurationMs = time.Since(start).Milliseconds()
		switch err {
		case nil:
			resp.Status = "ok"
		case ErrActiveInProgress:
			resp.Status = "busy"
			// Force a BUSY result to be returned to the caller
			resp.BestSolver = &SolverResult{
				Name:       "N/A",
				Status:     "BUSY",
				DurationUs: 0,
			}
		case ErrNoImprovingSolutionFromAnySolver, ErrNoPendingPodsToSchedule, ErrNoPendingPods:
			resp.Status = "noop"
			// Force a NOOP result to be returned to the caller
			if resp.BestSolver == nil {
				resp.BestSolver = &SolverResult{
					Name:       "N/A",
					Status:     "NOOP",
					DurationUs: 0,
				}
			} else {
				resp.BestSolver.Status += "-NOOP"
			}
		default:
			resp.Status = "error"
			resp.Error = err.Error()
		}
		writeJSON(w, http.StatusOK, resp)
	})

	server := &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	klog.InfoS("HTTP server started", "addr", addr)

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

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
