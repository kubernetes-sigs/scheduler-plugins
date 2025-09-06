package mycrossnodepreemption

import "sort"

func buildClusterState(in SolverInput) (map[string]*nLite, map[string]*pLite, []*pLite, []*nLite, *pLite) {
	// Build nodes map
	nodes := make(map[string]*nLite, len(in.Nodes))
	order := make([]*nLite, 0, len(in.Nodes))
	for i := range in.Nodes {
		n := &nLite{
			Name:         in.Nodes[i].Name,
			CapCPUm:      in.Nodes[i].CPUm,
			CapMemBytes:  in.Nodes[i].MemBytes,
			FreeCPUm:     in.Nodes[i].CPUm,
			FreeMemBytes: in.Nodes[i].MemBytes,
			Pods:         make(map[string]*pLite, 32),
		}
		nodes[n.Name] = n
		order = append(order, n)
	}
	sort.Slice(order, func(i, j int) bool { return order[i].Name < order[j].Name })

	// Build pods map and assign pods to nodes
	pods := make(map[string]*pLite, len(in.Pods)+1)
	pendingPods := make([]*pLite, 0, len(in.Pods))
	var pre *pLite
	// Add the preemptor to the total set of pods if it exists
	if in.Preemptor != nil {
		pendingPods = append(pendingPods, &pLite{
			UID:       in.Preemptor.UID,
			CPUm:      in.Preemptor.CPU_m,
			MemBytes:  in.Preemptor.MemBytes,
			Priority:  in.Preemptor.Priority,
			Protected: in.Preemptor.Protected,
		})
		pre = &pLite{
			UID:       in.Preemptor.UID,
			CPUm:      in.Preemptor.CPU_m,
			MemBytes:  in.Preemptor.MemBytes,
			Priority:  in.Preemptor.Priority,
			Protected: in.Preemptor.Protected,
		}
		pods[pre.UID] = pre
	}
	// Add also other pods to the total set of pods
	for i := range in.Pods {
		sp := in.Pods[i]
		if sp.Where == "" { // pending => treat as incoming
			pendingPods = append(pendingPods, &pLite{
				UID:       sp.UID,
				CPUm:      sp.CPU_m,
				MemBytes:  sp.MemBytes,
				Priority:  sp.Priority,
				Protected: sp.Protected,
			})
		}
		p := &pLite{
			UID:       sp.UID,
			CPUm:      sp.CPU_m,
			MemBytes:  sp.MemBytes,
			Priority:  sp.Priority,
			Protected: sp.Protected,
			Node:      sp.Where,
			origNode:  sp.Where,
		}
		pods[p.UID] = p

		// Add the pod to its node
		if p.Node != "" {
			if node := nodes[p.Node]; node != nil {
				node.addPod(p)
			}
		}
	}

	return nodes, pods, pendingPods, order, pre
}

func clusterTotalFree(order []*nLite) (cpu, mem int64) {
	for _, n := range order {
		cpu += n.FreeCPUm
		mem += n.FreeMemBytes
	}
	return
}

func spaceForIncoming(requestedCPUm, requestedMemBytes, freeCPUm, freeMemBytes int64) bool {
	return freeCPUm >= requestedCPUm && freeMemBytes >= requestedMemBytes
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
