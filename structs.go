// Think we can rename this and restructure it a bit?
type PendingSnapshot struct {
	PendingUIDs  map[types.UID]struct{}
	PendingCount int
	Fingerprint  string     // clusterFingerprint(nodes, pods)
	Pods         []*v1.Pod  // live snapshot (for priority checks)
	Nodes        []*v1.Node // live snapshot (for solver input)
}

type OptimizeLoopConfig struct {
	Label          string        // log label
	Interval       time.Duration // base tick interval
	InterludeDelay time.Duration // 0 => no "idle window"; >0 => require this long of stability
	CancelOnChange bool          // cancel in-flight run if pending set changes
}

type Plan struct {
	// Evicted pods
	Evicts []Placement `json:"evicts"`
	// Moved pods
	Moves []NewPlacement `json:"moves"`
	// All pods and their old placements
	OldPlacements []Placement `json:"old_placements"`
	// All pods and their new placements
	NewPlacements []NewPlacement `json:"new_placements"`
	// Placement by name for standalone pods: ns/name -> node
	PlacementByName map[string]string `json:"placement_by_name"`
	// Workload quotas for new placed pods that are part of a workload
	WorkloadQuotas WorkloadQuotas `json:"workload_quotas"`
	// Nominated node for the preemptor pod (if any)
	NominatedNode string `json:"nominated_node,omitempty"`
}

// WorkloadQuotas is a map of workloadKey -> node -> remaining count
type WorkloadQuotas map[string]map[string]int32

// StoredPlan represents the plan to be executed.
type StoredPlan struct {
	// Plugin version that generated the plan
	PluginVersion string `json:"plugin_version"`
	// The optimization mode used
	OptimizationStrategy string `json:"optimization_strategy"`
	// When the plan was generated
	GeneratedAt time.Time `json:"generated_at"`
	// When the plan was completed (if ever)
	CompletedAt *time.Time `json:"completed_at,omitempty"`
	// Solver summary (status & score)
	Solver SolverResult `json:"solver"`
	// Single-preemptor metadata
	Preemptor *Pod `json:"preemptor,omitempty"`
	// PlanStatus of the plan
	PlanStatus PlanStatus `json:"plan_status"`
	// The actual plan
	Plan *Plan `json:"plan"`
}

// Node represents a node in the cluster for the solver.
type Node struct {
	// Name of the node
	Name string `json:"name"`
	// Total CPU capacity in millicores
	CapCPUm int64 `json:"cap_cpu_m"`
	// Total memory capacity in bytes
	CapMemBytes int64 `json:"cap_mem_bytes"`
	// Allocated (used) CPU in millicores (not serialized)
	AllocCPUm int64 `json:"-"`
	// Allocated (used) memory in bytes (not serialized)
	AllocMemBytes int64 `json:"-"`
	// Labels of the node
	Labels map[string]string `json:"labels,omitempty"`
	// Pods currently on the node (not serialized)
	Pods map[types.UID]*Pod `json:"-"`
}

// Pod represents a pod in the cluster.
type Pod struct {
	// Unique identifier for the pod
	UID types.UID `json:"uid,omitempty"`
	// Namespace of the pod
	Namespace string `json:"namespace,omitempty"`
	// Name of the pod
	Name string `json:"name,omitempty"`
	// Requested CPU in millicores
	ReqCPUm int64 `json:"req_cpu_m,omitempty"`
	// Requested memory in bytes
	ReqMemBytes int64 `json:"req_mem_bytes,omitempty"`
	// Priority of the pod
	Priority int32 `json:"priority,omitempty"`
	// Whether the pod is protected from preemption
	Protected bool `json:"protected,omitempty"`
	// Current node of the pod (empty if new pod)
	Node string `json:"node,omitempty"`
}

// NewPlacement represents a pod placement from one node to another.
type NewPlacement struct {
	// Pod being placed
	Pod Pod `json:"pod"`
	// Previous node of the pod; empty if new pod
	FromNode string `json:"from_node,omitempty"`
	// New node of the pod
	ToNode string `json:"to_node"`
}

// Placement represents a pod placement on a node.
type Placement struct {
	// Pod being placed
	Pod Pod `json:"pod"`
	// Node the pod is placed on
	Node string `json:"node"`
}

// SolverInput is the input to a solver.
type SolverInput struct {
	// Preemptor pod (if any; single pod mode)
	Preemptor *Pod `json:"preemptor,omitempty"`
	// Nodes to consider
	Nodes []Node `json:"nodes"`
	// Pods to schedule / re-schedule
	Pods []Pod `json:"pods"`
	// Timeout for the solver (ms)
	TimeoutMs int64 `json:"timeout_ms"`
	// If true, ignore affinity rules
	IgnoreAffinity bool `json:"ignore_affinity"`

	// TODO: Make python specific options an extra struct?
	// Log solver progress
	LogProgress bool `json:"log_progress,omitempty"`
	// Gap to optimality (0.0 = ignore)
	GapLimit float64 `json:"gap_limit,omitempty"`
	// Guaranteed fraction of time for all tiers (0.0-1.0)
	GuaranteedTierFraction float64 `json:"guaranteed_tier_fraction,omitempty"`
	// Fraction of a tier's budget for moves (0.0-1.0)
	MoveFractionOfTier float64 `json:"move_fraction_of_tier,omitempty"`
}

// SolverOutput is the output from a solver.
type SolverOutput struct {
	// Status of the solver (failed/infeasible/feasible/optimal)
	Status string `json:"status"`
	// Pod placements (new or moved)
	Placements []NewPlacement `json:"placements"`
	// Evicted pods
	Evictions []Placement `json:"evictions"`
	// Duration in milliseconds of the solver
	DurationMs int64 `json:"duration_ms,omitempty"`

	// Make python-specific fields optional
	// Stages of the solver
	Stages []SolverStage `json:"stages,omitempty"`
}

// SolverAttempt defines a solver attempt configuration and function.
type SolverAttempt struct {
	// Name of the solver attempt
	Name string
	// Whether the solver attempt is enabled
	Enabled bool
	// Timeout for the solver attempt
	Timeout time.Duration
	// Function to run the solver attempt
	Run func(ctx context.Context, in SolverInput) (*SolverOutput, error)
}

// SolverResult is the result of a solver attempt.
type SolverResult struct {
	// Name of the solver attempt
	Name string `json:"name,omitempty"`
	// Status of the solver
	// filled from Output.Status when present
	Status string `json:"status,omitempty"`
	// DurationMs of the solver
	DurationMs int64 `json:"duration_ms,omitempty"`
	// Score of the solution
	Score SolverScore `json:"score,omitempty"`
	// Stages of the solver
	Stages []SolverStage `json:"stages,omitempty"`
	// In-memory only (not exported)
	// Comparison vs previous leader (-1 worse, 0 tie, 1 better)
	CmpBase int `json:"-"`
	// Full detailed solver output (not exported)
	Output *SolverOutput `json:"-"`
}

// SolverScore of a solver solution
type SolverScore struct {
	// Number of pods placed by priority (higher is better)
	PlacedByPriority map[string]int `json:"placed_by_priority,omitempty"`
	// Number of evicted pods (lower is better)
	Evicted int `json:"evicted,omitempty"`
	// Number of moved pods (lower is better)
	Moved int `json:"moved,omitempty"`
}

type SolverStage struct {
	// Tier of the solver stage (0 = placement, 1..n = moves)
	Tier int `json:"tier"`
	// Name of the solver stage (e.g. "place" vs "moves")
	Stage string `json:"stage"`
	// Status of the solver stage
	Status string `json:"status"`
	// Duration of the solver stage
	DurationMs int64 `json:"duration_ms"`
	// The ratio gap to optimality (if known)
	RelativeGap string `json:"relative_gap,omitempty"`
}

// ExportedSolverStats is the structure used to export solver run statistics.
type ExportedSolverStats struct {
	// TimestampNs is the timestamp of the run in nanoseconds.
	TimestampNs int64 `json:"timestamp_ns"`
	// Error (if any)
	Error string `json:"error,omitempty"`
	// Best solver name
	BestName string `json:"best_name,omitempty"`
	// Baseline score
	Baseline *SolverScore `json:"baseline,omitempty"`
	// Best score
	Attempts []SolverResult `json:"attempts,omitempty"`
}
