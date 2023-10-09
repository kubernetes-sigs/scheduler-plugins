package laxity

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestPodLaxity(t *testing.T) {
	now := time.Now()
	pe := &podExecution{
		deadline:    now.Add(time.Second * 10),
		estExecTime: time.Second * 3,
	}
	expectedLaxity := 7 * time.Second
	laxity, err := pe.laxity()
	assert.NoError(t, err)
	assertLaxity(t, expectedLaxity, laxity)

	t.Log("sleep for 1 second, laxity should reduce by 1 second")
	time.Sleep(time.Second)
	expectedLaxity -= time.Second
	laxity, _ = pe.laxity()
	assertLaxity(t, expectedLaxity, laxity)

	t.Log("start pod execution")
	pe.start()
	t.Log("sleep for 1 second, laxity should remain the same")
	time.Sleep(time.Second)
	laxity, _ = pe.laxity()
	assertLaxity(t, expectedLaxity, laxity)

	t.Log("start pod execution again, laxity should not change")
	pe.start()
	laxity, _ = pe.laxity()
	assertLaxity(t, expectedLaxity, laxity)

	t.Log("pause pod execution")
	pe.pause()
	t.Log("sleep for 2 seconds, laxity should reduce by 2 seconds")
	time.Sleep(time.Second * 2)
	expectedLaxity -= time.Second * 2
	laxity, _ = pe.laxity()
	assertLaxity(t, expectedLaxity, laxity)

	t.Log("get laxity again, laxity should remain the same")
	laxity, _ = pe.laxity()
	assertLaxity(t, expectedLaxity, laxity)

	t.Log("start pod execution after pausing")
	pe.start()
	t.Log("sleep for 1 second, laxity should remain the same")
	time.Sleep(time.Second)
	laxity, _ = pe.laxity()
	assertLaxity(t, expectedLaxity, laxity)

	t.Log("sleep for 2 seconds, beyond estimated execution time")
	time.Sleep(time.Second * 2)
	laxity, err = pe.laxity()
	assert.Error(t, err, ErrBeyondEstimation)
	assertLaxity(t, 0, laxity)
}

func assertLaxity(t *testing.T, expected, actual time.Duration) {
	assert.LessOrEqual(t, actual, expected)
	assert.GreaterOrEqual(t, actual, expected-time.Millisecond*50)
}
