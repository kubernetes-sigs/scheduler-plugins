package estimator

import (
	"time"
)

/*
#cgo LDFLAGS: -lm
#include "llsp.h"
*/
import "C"

type Metrics []float64

type Estimator interface {
	Add(metrics Metrics, actualExecTime time.Duration)
	EstimateExecTime(metrics Metrics) time.Duration
}

// TODO: integrate with ATLAS C lib
type atlasEstimator struct {
	msize  int
	solver *C.llsp_t
}

func NewATLASEstimator(metricSize int) Estimator {
	return &atlasEstimator{
		msize:  metricSize,
		solver: C.llsp_new(C.size_t(metricSize)),
	}
}

func (a *atlasEstimator) padMetrics(metrics Metrics) Metrics {
	maxSize := 4
	if size := len(metrics); size < maxSize {
		maxSize = size
	}
	var m []float64
	m = append(m, metrics[:maxSize]...)
	// add zeroes as padding
	for i := len(metrics) - 1; i < a.msize; i++ {
		m = append(m, 0)
	}
	return m
}

func (a *atlasEstimator) Add(metrics Metrics, actualExecTime time.Duration) {
	C.llsp_add(a.solver, (*C.double)(&a.padMetrics(metrics)[0]), C.double(actualExecTime))
	C.llsp_solve(a.solver)
}

// EstimateExecTime returns the estimated execution time based on a set of metrics
func (a *atlasEstimator) EstimateExecTime(metrics Metrics) time.Duration {
	prediction := C.llsp_predict(a.solver, (*C.double)(&a.padMetrics(metrics)[0]))
	return time.Duration(prediction)
}
