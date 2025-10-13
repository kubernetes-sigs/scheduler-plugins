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
	Status        string         `json:"status"`
	DurationMs    int64          `json:"duration_ms"`
	Error         string         `json:"error,omitempty"`
	Active        bool           `json:"active"`
	Baseline      *SolverScore   `json:"baseline,omitempty"`
	BestName      string         `json:"best_name,omitempty"`
	Attempts      []SolverResult `json:"attempts,omitempty"`
	PendingBefore int            `json:"pending_before"`
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

	mux.HandleFunc("/solve", func(w http.ResponseWriter, r *http.Request) {
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

		_, baseline, bestName, _, attempts, err := pl.runFlow(context.Background(), nil)
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
