// http_types.go
package mypriorityoptimizer

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
