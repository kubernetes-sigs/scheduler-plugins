// Package store implements a simple in-memory database, to be used in simple cases
package store

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

var ErrNotFound = errors.New("pod group not found in store")

type PgInfo struct {
	Zone  string
	Added time.Time
}

type InMemoryStore struct {
	mu   sync.RWMutex
	data map[string]PgInfo
}

func NewInMemoryStore() *InMemoryStore {
	return &InMemoryStore{
		data: make(map[string]PgInfo),
		mu:   sync.RWMutex{},
	}
}

func (s *InMemoryStore) Add(key, val string, ts time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, ok := s.data[key]
	if !ok {
		s.data[key] = PgInfo{
			Zone:  val,
			Added: ts,
		}
		return true
	}
	return false
}

func (s *InMemoryStore) Get(key string) (PgInfo, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	_, ok := s.data[key]
	if !ok {
		return PgInfo{}, fmt.Errorf("%w: %s", ErrNotFound, key)
	}

	return s.data[key], nil
}

func (s *InMemoryStore) Has(key string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	_, ok := s.data[key]
	return ok
}

func (s *InMemoryStore) Delete(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.data, key)
}

func (s *InMemoryStore) Clean() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.data = make(map[string]PgInfo)
}
