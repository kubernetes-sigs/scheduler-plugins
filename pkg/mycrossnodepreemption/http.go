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
	Status        string `json:"status"`
	Message       string `json:"message,omitempty"`
	DurationMs    int64  `json:"durationMs"`
	Active        bool   `json:"active"`
	Error         string `json:"error,omitempty"`
	PendingBefore int    `json:"pendingBefore"`
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
			resp.Message = "caches not warmed up"
			resp.DurationMs = time.Since(start).Milliseconds()
			writeJSON(w, http.StatusPreconditionFailed, resp)
			return
		}

		pods, _ := pl.getPods()
		resp.PendingBefore = countPendingPods(pods)

		_, err := pl.runFlow(context.Background(), nil)
		resp.DurationMs = time.Since(start).Milliseconds()
		switch err {
		case nil:
			resp.Status = "ok"
		case ErrActiveInProgress:
			resp.Status = "busy"
			resp.Error = ErrActiveInProgress.Error()
		case ErrNoop:
			resp.Status = "noop"
		case ErrNoSolverSolution:
			resp.Status = "no-solution"
			resp.Error = ErrNoSolverSolution.Error()
		default:
			resp.Status = "ok"
		}
		writeJSON(w, http.StatusOK, resp)
	})

	server := &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	klog.InfoS("HTTP server started", "addr", addr, "mode", strategyToString())

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
