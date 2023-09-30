package priorityqueue_test

import (
	"fmt"
	"math/rand"
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"sigs.k8s.io/scheduler-plugins/pkg/rtpreemptive/priorityqueue"
)

func TestPriorityQueue(t *testing.T) {
	q := priorityqueue.New(0)
	item1 := priorityqueue.NewItem("a", 10)
	item2 := priorityqueue.NewItem("b", -10)
	item3 := priorityqueue.NewItem("c", 30)
	item4 := priorityqueue.NewItem("d", 0)
	q.PushItem(item1)
	q.PushItem(item2)
	q.PushItem(item3)
	q.PushItem(item4)
	assert.Equal(t, item1, q.GetItem(item1.Value()))
	assert.Equal(t, item2, q.GetItem(item2.Value()))
	assert.Equal(t, item1, q.RemoveItem(item1.Value()))
	assert.Nil(t, q.GetItem(item1.Value()))
	assert.Equal(t, item3, q.PopItem())
	assert.Equal(t, item4, q.PopItem())
	assert.Equal(t, item2, q.PopItem())
	assert.Nil(t, q.PopItem())
	assert.Equal(t, q.Size(), 0)
}

func TestPriorityQueueConcurrent(t *testing.T) {
	q := priorityqueue.New(0)
	priorities := []int64{10, -10, 0, 20, 30, 5, 7, 100, -99}
	popAttempts, successPops := 5, 0
	var wg sync.WaitGroup
	for i, p := range priorities {
		wg.Add(1)
		item := priorityqueue.NewItem(strconv.Itoa(i), p)
		go func() {
			q.PushItem(item)
			wg.Done()
		}()
	}
	for i := 0; i < popAttempts; i++ {
		wg.Add(1)
		go func() {
			if q.PopItem() != nil {
				successPops++
			}
			wg.Done()
		}()
	}
	wg.Wait()
	assert.Equal(t, len(priorities)-successPops, q.Size())
	prevPrio, currPrio := q.PopItem().Priority(), int64(0)
	for i := 0; i < q.Size()-1; i++ {
		currPrio = q.PopItem().Priority()
		assert.LessOrEqual(t, currPrio, prevPrio)
		prevPrio = int64(currPrio)
	}
}

func BenchmarkPriorityQueuePushItem(b *testing.B) {
	q := priorityqueue.New(0)
	b.ResetTimer()
	// run the Fib function b.N times
	for n := 0; n < b.N; n++ {
		item := priorityqueue.NewItem(fmt.Sprintf("item-%d", n), rand.Int63n(int64(b.N)))
		q.PushItem(item)
	}
}

func BenchmarkPriorityQueuePopItem(b *testing.B) {
	q := priorityqueue.New(0)
	for n := 0; n < b.N; n++ {
		item := priorityqueue.NewItem(fmt.Sprintf("item-%d", n), rand.Int63n(int64(b.N)))
		q.PushItem(item)
	}
	b.ResetTimer()
	// run the Fib function b.N times
	for n := 0; n < b.N; n++ {
		q.PopItem()
	}
}
