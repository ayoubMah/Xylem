package main

import (
	"sync"
)

type KVStore struct {
	mu   sync.RWMutex
	data map[string][]byte
}

func NewKVStore() *KVStore {
	return &KVStore{
		data: make(map[string][]byte),
	}
}

func (s *KVStore) Set(key string, value []byte) {
	// imp
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[key] = value
}

func (s *KVStore) Get(key string) ([]byte, bool) {
	// imp
	s.mu.RLock()
	defer s.mu.RUnlock()
	val, ok := s.data[key]
	return val, ok
}

func (s *KVStore) Delete(key string) {
	// imp
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, key)
}
