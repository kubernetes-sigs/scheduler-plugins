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

type SolveMode int

const (
	SolveCohort SolveMode = iota
	SolveSingle
)

func solverFeasible(out *SolverOutput) bool {
	return out != nil && (out.Status == "OPTIMAL" || out.Status == "FEASIBLE")
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
		// Use snapshot view (keeps scheduler’s picture coherent for the cycle)
		l := pl.Handle.SnapshotSharedLister()
		if l == nil {
			return SolverInput{}, fmt.Errorf("no snapshot lister")
		}
		nodeInfos, err := l.NodeInfos().List()
		if err != nil {
			return SolverInput{}, fmt.Errorf("list nodes: %w", err)
		}

		pre := toSolverPod(preemptor, "")
		in.Preemptor = &pre

		usable := map[string]bool{}
		for _, ni := range nodeInfos {
			if ni == nil || ni.Node() == nil {
				continue
			}
			n := ni.Node()
			isCP := n.Labels["node-role.kubernetes.io/control-plane"] != "" ||
				n.Labels["node-role.kubernetes.io/master"] != "" ||
				n.Name == "control-plane" || n.Name == "kind-control-plane"
			if isCP || n.Spec.Unschedulable || ni.Allocatable.MilliCPU <= 0 || ni.Allocatable.Memory <= 0 {
				continue
			}
			in.Nodes = append(in.Nodes, SolverNode{
				Name:     n.Name,
				CPUm:     ni.Allocatable.MilliCPU,
				MemBytes: ni.Allocatable.Memory,
			})
			usable[n.Name] = true
		}
		if len(in.Nodes) == 0 {
			return SolverInput{}, fmt.Errorf("no usable nodes available (snapshot)")
		}
		for _, ni := range nodeInfos {
			if ni == nil || ni.Node() == nil {
				continue
			}
			for _, pi := range ni.Pods {
				where := pi.Pod.Spec.NodeName
				if where != "" && !usable[where] {
					continue
				}
				sp := toSolverPod(pi.Pod, where)
				if pi.Pod.Namespace == "kube-system" {
					sp.Protected = true
				}
				// Do NOT duplicate the preemptor
				if sp.UID == string(preemptor.UID) {
					continue
				}
				in.Pods = append(in.Pods, sp)
			}
		}

	case SolveCohort:
		// Use live listers for batch
		nodes, err := pl.getNodes()
		if err != nil {
			return SolverInput{}, fmt.Errorf("list nodes: %w", err)
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
		if len(in.Nodes) == 0 {
			return SolverInput{}, fmt.Errorf("no usable nodes available")
		}

		allPods, err := pl.getPods()
		if err != nil {
			return SolverInput{}, fmt.Errorf("list pods: %w", err)
		}
		seen := make(map[string]bool, len(allPods)+len(batched))
		for _, p := range allPods {
			where := p.Spec.NodeName
			if where != "" && !usable[where] {
				continue
			}
			sp := toSolverPod(p, where)
			if p.Namespace == "kube-system" {
				sp.Protected = true
			}
			in.Pods = append(in.Pods, sp)
			seen[sp.UID] = true
		}
		for _, p := range batched {
			sp := toSolverPod(p, "")
			if !seen[sp.UID] {
				in.Pods = append(in.Pods, sp)
				seen[sp.UID] = true
			}
		}

	default:
		return SolverInput{}, fmt.Errorf("unknown solve mode")
	}

	return in, nil
}

func (pl *MyCrossNodePreemption) runSolver(ctx context.Context, in SolverInput) (*SolverOutput, error) {
	raw, _ := json.Marshal(in)
	klog.V(2).InfoS("Solver input", "nodes", len(in.Nodes), "pods", len(in.Pods), "hasPreemptor", in.Preemptor != nil)

	cmd := exec.CommandContext(ctx, "python3", PythonSolverPath)
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
			klog.V(2).Info("solver: " + s.Text())
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

// ----------- Solver Helpers --------------

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
