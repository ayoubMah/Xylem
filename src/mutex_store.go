package main

import "sync"

type MutexKVStore struct {
	mu   sync.Mutex
	data map[string][]byte
}

func NewMutexKVStore(capacity int) *MutexKVStore {
	return &MutexKVStore{
		data: make(map[string][]byte, capacity),
	}
}

func (s *MutexKVStore) Set(key string, value []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[key] = value
}

func (s *MutexKVStore) Get(key string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	val, ok := s.data[key]
	return val, ok
}

func (s *MutexKVStore) Delete(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, key)
}
