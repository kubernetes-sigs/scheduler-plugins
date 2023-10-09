package estimator

import (
	"math/rand"
	"time"

	"k8s.io/klog/v2"
)

type Metrics map[string]interface{}

type Estimator interface {
	EstimateExecTime(metrics Metrics) time.Duration
}

// TODO: integrate with ATLAS C lib
type atlasEstimator struct{}

func NewATLASEstimator() Estimator {
	return &atlasEstimator{}
}

// EstimateExecTime returns the estimated execution time based on a set of metrics
func (a *atlasEstimator) EstimateExecTime(metrics Metrics) time.Duration {
	// TODO: fix implementation
	execTime, ok := metrics["exec_time"]
	if ok {
		return execTime.(time.Duration)
	}
	ddl, ok := metrics["ddl"]
	if !ok {
		return 0
	}
	max, min := 100, 70
	factor := rand.Intn(max-min) + min
	estExecTime := ddl.(time.Duration) * (100 - time.Duration(factor)) / 100
	klog.InfoS("ATLAS Estimator", "factor", float32(factor)/100, "ddl", ddl, "estimated", estExecTime)
	// ENDTODO
	return estExecTime
}
