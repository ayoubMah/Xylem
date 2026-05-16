package main

type KVStore struct {
	data map[string][]byte
}

func NewKVStore() *KVStore {
	return &KVStore{
		data: make(map[string][]byte),
	}
}

func (s *KVStore) Set(key string, value []byte) {
	// imp
	s.data[key] = value
}

func (s *KVStore) Get(key string) ([]byte, bool) {
	// imp
	val, ok := s.data[key]
	return val, ok
}

func (s *KVStore) Delete(key string) {
	// imp
	delete(s.data, key)
}
