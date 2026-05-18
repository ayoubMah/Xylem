package main

type UnsafeKVStore struct {
	data map[string][]byte
}

func NewUnsafeKVStore() *UnsafeKVStore {
	return &UnsafeKVStore{
		data: make(map[string][]byte),
	}
}

func (s *UnsafeKVStore) Set(key string, value []byte) {
	s.data[key] = value
}

func (s *UnsafeKVStore) Get(key string) ([]byte, bool) {
	val, ok := s.data[key]
	return val, ok
}
