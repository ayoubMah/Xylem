package main

import "testing"

func BenchmarkMutexSet(b *testing.B) {
	store := NewMutexKVStore(b.N)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		store.Set("key", []byte("value"))
	}
}

func BenchmarkMutexGet(b *testing.B) {
	store := NewMutexKVStore(1)
	store.Set("key", []byte("value"))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		store.Get("key")
	}
}

func BenchmarkMutexGetParallel(b *testing.B) {
	store := NewMutexKVStore(1)
	store.Set("key", []byte("value"))
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			store.Get("key")
		}
	})
}
