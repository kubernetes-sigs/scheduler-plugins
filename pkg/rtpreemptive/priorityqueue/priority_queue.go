package priorityqueue

import (
	"container/heap"
	"sync"
)

// An Item is something we manage in a priority queue.
type Item struct {
	value    string // The value of the item; arbitrary.
	priority int64  // The priority of the item in the queue.
	// The index is needed by update and is maintained by the heap.Interface methods.
	index int // The index of the item in the heap.
}

func (i *Item) Value() string {
	return i.value
}

func (i *Item) Priority() int64 {
	return i.priority
}

func NewItem(value string, priority int64) *Item {
	return &Item{
		value:    value,
		priority: priority,
	}
}

// A PriorityQueue interface defines operation can be called on the queue
type PriorityQueue interface {
	Size() int
	PopItem() *Item
	PushItem(item *Item)
	RemoveItem(value string) *Item
	GetItem(value string) *Item
}

type priorityQueueImpl struct {
	sync.RWMutex
	items []*Item
}

func New(len int) PriorityQueue {
	pq := &priorityQueueImpl{
		items: make([]*Item, len),
	}
	heap.Init(pq)
	return pq
}

func (pq *priorityQueueImpl) PopItem() *Item {
	pq.Lock()
	defer pq.Unlock()
	if pq.Len() <= 0 {
		return nil
	}
	return heap.Pop(pq).(*Item)
}

func (pq *priorityQueueImpl) PushItem(item *Item) {
	pq.Lock()
	defer pq.Unlock()
	heap.Push(pq, item)
}

func (pq *priorityQueueImpl) RemoveItem(value string) *Item {
	pq.Lock()
	defer pq.Unlock()
	idx := -1
	for _, item := range pq.items {
		if item.value == value {
			idx = item.index
		}
	}
	if idx >= 0 {
		return heap.Remove(pq, idx).(*Item)
	}
	return nil
}

func (pq *priorityQueueImpl) GetItem(value string) *Item {
	pq.RLock()
	defer pq.RUnlock()
	for _, item := range pq.items {
		if item.value == value {
			return item
		}
	}
	return nil
}

func (pq *priorityQueueImpl) Size() int {
	pq.RLock()
	defer pq.RUnlock()
	return pq.Len()
}
func (pq *priorityQueueImpl) Len() int {
	return len(pq.items)
}

func (pq *priorityQueueImpl) Less(i, j int) bool {
	// We want Pop to give us the highest, not lowest, priority so we use greater than here.
	return pq.items[i].priority > pq.items[j].priority
}

func (pq *priorityQueueImpl) Swap(i, j int) {
	pq.items[i], pq.items[j] = pq.items[j], pq.items[i]
	pq.items[i].index = i
	pq.items[j].index = j
}

func (pq *priorityQueueImpl) Push(x any) {
	n := len(pq.items)
	item := x.(*Item)
	item.index = n
	pq.items = append(pq.items, item)
}

func (pq *priorityQueueImpl) Pop() any {
	old := pq.items
	n := len(old)
	item := old[n-1]
	old[n-1] = nil  // avoid memory leak
	item.index = -1 // for safety
	pq.items = old[0 : n-1]
	return item
}
