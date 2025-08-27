// solve.go
package mycrossnodepreemption

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

// TODO: Build solve, buildSolverInput and runSolver into one more efficient function
// TODO: Think cohort and single code can be reduced.

func (pl *MyCrossNodePreemption) solve(
	ctx context.Context,
	mode SolveMode,
	preemptor *v1.Pod,
	batched []*v1.Pod,
	timeout time.Duration,
) (*SolverOutput, error) {
	in, err := pl.buildSolverInput(mode, preemptor, batched, timeout)
	if err != nil {
		return nil, err
	}
	return pl.runSolver(ctx, in)
}

// buildSolverInput builds the common input for either batch(cohort) or single-preemptor.
func (pl *MyCrossNodePreemption) buildSolverInput(
	mode SolveMode,
	preemptor *v1.Pod, // only for SolveSingle
	batched []*v1.Pod, // only for SolveCohort
	timeout time.Duration,
) (SolverInput, error) {
	in := SolverInput{
		TimeoutMs:      timeout.Milliseconds(),
		IgnoreAffinity: true,
		LogProgress:    SolverLogProgress,
		Nodes:          make([]SolverNode, 0),
		Pods:           make([]SolverPod, 0),
		Mode:           SolverMode,
	}

	switch mode {
	case SolveSingle:
		if preemptor == nil {
			return SolverInput{}, fmt.Errorf("SolveSingle requires preemptor")
		}
		pre := toSolverPod(preemptor, "")
		in.Preemptor = &pre

		// choose snapshot or factory; both pass includePending=false
		if err := pl.fillFromFactory(&in, preemptor, nil, false); err != nil {
			return SolverInput{}, fmt.Errorf("fill (single): %w", err)
		}

	case SolveCohort:
		// include other pending pods (the batched set)
		if err := pl.fillFromFactory(&in, nil, batched, true); err != nil {
			return SolverInput{}, fmt.Errorf("fill (cohort): %w", err)
		}

	default:
		return SolverInput{}, fmt.Errorf("unknown solve mode")
	}

	// If zero Ready/usable nodes were seen via snapshot, poll snapshot briefly and try again.
	// TODO: make this better
	if len(in.Nodes) == 0 {
		klog.InfoS("buildSolverInput: zero usable nodes via snapshot; waiting for nodes to become Ready")
		const maxWait = 10 * time.Second
		const step = 2 * time.Second
		time.Sleep(step)
		deadline := time.Now().Add(maxWait)

		for time.Now().Before(deadline) && len(in.Nodes) == 0 {
			in.Nodes = in.Nodes[:0]
			in.Pods = in.Pods[:0]
			if err := pl.fillFromFactory(&in, preemptor, batched, false); err != nil {
				klog.V(3).InfoS("retry snapshot fill failed", "err", err)
			}
			if len(in.Nodes) > 0 {
				break
			}
			time.Sleep(step)
		}
	}

	if len(in.Nodes) == 0 {
		return SolverInput{}, fmt.Errorf("no usable Ready nodes available (snapshot)")
	}
	return in, nil
}

// ---------- helpers ----------

// fillFromFactory adds nodes/pods using SharedInformerFactory listers (live).
// If batched != nil, pending batched pods are appended with where="" (and preemptor can be nil).
func (pl *MyCrossNodePreemption) fillFromFactory(
	in *SolverInput,
	preemptor *v1.Pod,
	batched []*v1.Pod,
	includePending bool,
) error {
	// Nodes
	nodes, err := pl.getNodes()
	if err != nil {
		return fmt.Errorf("list nodes (factory): %w", err)
	}
	usable := map[string]bool{}
	for _, n := range nodes {
		if !isNodeUsable(n) {
			continue
		}
		in.Nodes = append(in.Nodes, SolverNode{
			Name:     n.Name,
			CPUm:     n.Status.Allocatable.Cpu().MilliValue(),
			MemBytes: n.Status.Allocatable.Memory().Value(),
		})
		usable[n.Name] = true
	}

	// Pods
	allPods, err := pl.getPods()
	if err != nil {
		return fmt.Errorf("list pods (factory): %w", err)
	}
	preUID := ""
	if preemptor != nil {
		preUID = string(preemptor.UID)
	}
	seen := make(map[string]bool, len(allPods)+len(batched))
	for _, p := range allPods {
		// Skip the preemptor (it is provided via in.Preemptor)
		if string(p.UID) == preUID {
			continue
		}

		where := p.Spec.NodeName
		if where == "" {
			// Only include pending pods when explicitly asked (cohort mode).
			if !includePending {
				continue
			}
		} else {
			// If the pod is already bound to a node, ensure that node is usable.
			if !usable[where] {
				continue
			}
		}

		sp := toSolverPod(p, where)
		if p.Namespace == "kube-system" {
			sp.Protected = true
		}
		if !seen[sp.UID] {
			in.Pods = append(in.Pods, sp)
			seen[sp.UID] = true
		}
	}

	// Cohort: append the batched pending pods (where = ""), if requested
	if includePending {
		for _, p := range batched {
			if p == nil {
				continue
			}
			if string(p.UID) == preUID {
				continue
			}
			sp := toSolverPod(p, "")
			if p.Namespace == "kube-system" {
				sp.Protected = true
			}
			if !seen[sp.UID] {
				in.Pods = append(in.Pods, sp)
				seen[sp.UID] = true
			}
		}
	}
	return nil
}

func (pl *MyCrossNodePreemption) runSolver(ctx context.Context, in SolverInput) (*SolverOutput, error) {
	raw, _ := json.Marshal(in)
	klog.V(V2).InfoS("Solver input", "nodes", len(in.Nodes), "pods", len(in.Pods), "hasPreemptor", in.Preemptor != nil)

	cmd := exec.CommandContext(ctx, "python3", SolverPath)
	cmd.Stdin = bytes.NewReader(raw)

	// 1) Pipe stdout (JSON) and stderr (logs) separately
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}

	// 2) Stream stderr to klog line-by-line
	go func() {
		s := bufio.NewScanner(stderr)
		// bump buffer in case OR-Tools prints long lines
		buf := make([]byte, 0, 256*1024)
		s.Buffer(buf, 1024*1024)
		for s.Scan() {
			klog.V(V2).Info("solver: " + s.Text())
		}
		if err := s.Err(); err != nil {
			klog.Info("solver scan failed: " + err.Error())
		}
	}()

	// 3) Start and wait
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("solver start: %w", err)
	}

	// 4) Read full JSON from stdout
	outBuf, err := io.ReadAll(stdout)
	if err != nil {
		_ = cmd.Wait()
		return nil, fmt.Errorf("read solver stdout: %w", err)
	}

	if err := cmd.Wait(); err != nil {
		// Non‑zero exit; logs already streamed above
		return nil, fmt.Errorf("solver run: %w", err)
	}

	var out SolverOutput
	if err := json.Unmarshal(outBuf, &out); err != nil {
		return nil, fmt.Errorf("decode solver output: %w", err)
	}
	if !solverFeasible(&out) {
		return &out, fmt.Errorf("solver status: %s", out.Status)
	}
	return &out, nil
}

func toSolverPod(p *v1.Pod, where string) SolverPod {
	return SolverPod{
		UID:       string(p.UID),
		Namespace: p.Namespace,
		Name:      p.Name,
		CPU_m:     getPodCPURequest(p),
		MemBytes:  getPodMemoryRequest(p),
		Priority:  getPodPriority(p),
		Where:     where,
	}
}
