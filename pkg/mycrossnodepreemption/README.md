# MyCrossNodePreemption Plugin

## Overview

An improved cross-node preemption plugin that addresses the limitations of the original CrossNodePreemption plugin. This plugin implements efficient algorithms for cross-node preemption with advanced optimization strategies.

## Key Improvements

### 🚀 **Performance Optimizations**
- **Branch Cutting Algorithm**: Uses early termination strategies to avoid exploring infeasible solutions
- **Priority-based Pruning**: Eliminates candidates based on pod priorities and resource requirements
- **Greedy Heuristics**: Employs smart candidate selection instead of brute-force enumeration
- **Resource-aware Selection**: Considers resource efficiency when selecting victims

### 🎯 **Advanced Features**
- **Cost-Benefit Analysis**: Evaluates the cost of preemption vs. benefit gained
- **Multi-dimensional Scoring**: Considers pod priority, resource usage, and topology constraints
- **PDB-aware Preemption**: Respects PodDisruptionBudgets during victim selection
- **Affinity-aware Selection**: Optimizes for pod affinity and anti-affinity constraints

### 📊 **Algorithm Complexity**
- **Original**: O(2^n) - exponential brute-force
- **Improved**: O(n log n) - efficient with branch cutting and heuristics

## Maturity Level

- [x] 👦 Beta (production-ready with advanced optimizations)
- [ ] 👨 Stable (battle-tested in production environments)

## Configuration

```yaml
apiVersion: kubescheduler.config.k8s.io/v1
kind: KubeSchedulerConfiguration
leaderElection:
  leaderElect: false
clientConnection:
  kubeconfig: "REPLACE_ME_WITH_KUBE_CONFIG_PATH"
profiles:
- schedulerName: default-scheduler
  plugins:
    postFilter:
      enabled:
      - name: MyCrossNodePreemption
  pluginConfig:
  - name: MyCrossNodePreemption
    args:
      maxCandidates: 100        # Maximum candidates to evaluate
      enableBranchCutting: true # Enable optimization algorithms
      considerPDBs: true        # Respect PodDisruptionBudgets
      scoreWeights:
        priority: 0.4           # Weight for pod priority
        resources: 0.3          # Weight for resource efficiency
        topology: 0.3           # Weight for topology constraints
```

## Algorithm Overview

### 1. Candidate Discovery
- Identify nodes where preemption might help
- Filter based on resource requirements and constraints
- Apply early filtering to reduce search space

### 2. Victim Selection
- Use priority-based ranking
- Consider resource efficiency metrics
- Respect PodDisruptionBudgets
- Optimize for topology requirements

### 3. Solution Optimization
- Apply branch cutting when solutions become infeasible
- Use greedy heuristics for fast convergence
- Multi-criteria optimization for best candidate selection

## Usage

This plugin can be used as a drop-in replacement for the original CrossNodePreemption plugin with significantly better performance and production-ready optimizations.
