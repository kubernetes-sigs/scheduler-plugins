package laxity

import (
	"errors"
	"sync"
	"time"
)

var (
	ErrOverdue          = errors.New("already passed deadline")
	ErrBeyondEstimation = errors.New("actual execution time is beyond initial estimation")
)

type podExecution struct {
	sync.Mutex
	deadline       time.Time
	estExecTime    time.Duration
	actualExecTime time.Duration
	runningSince   time.Time
	running        bool
}

func (p *podExecution) start() {
	p.Lock()
	defer p.Unlock()
	// if previously paused, update running since time
	if !p.running {
		p.runningSince = time.Now()
	}
	p.running = true
}

func (p *podExecution) pause() {
	p.Lock()
	defer p.Unlock()
	// if previous running, update actual execution time
	if p.running {
		p.actualExecTime += time.Since(p.runningSince)
	}
	p.running = false
}

func (p *podExecution) laxity() (time.Duration, error) {
	p.Lock()
	defer p.Unlock()
	if p.running {
		p.actualExecTime += time.Since(p.runningSince)
		p.runningSince = time.Now()
	}
	timeToDDL := time.Until(p.deadline)
	if timeToDDL < 0 {
		return 0, ErrOverdue
	}
	remainingExecTime := p.estExecTime - p.actualExecTime
	if remainingExecTime < 0 {
		return 0, ErrBeyondEstimation
	}
	if laxity := timeToDDL - remainingExecTime; laxity >= 0 {
		return laxity, nil
	}
	return 0, nil
}
