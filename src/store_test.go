package main

import "testing"

func BenchmarkSet(b *testing.B) {
	store := NewKVStore(b.N)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		store.Set("key", []byte("value"))
	}
}

func BenchmarkGet(b *testing.B) {
	store := NewKVStore(1)
	store.Set("key", []byte("value"))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		store.Get("key")
	}
}

func BenchmarkSetParallel(b *testing.B) {
	store := NewKVStore(b.N)
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			store.Set("key", []byte("value"))
		}
	})
}

func BenchmarkGetParallel(b *testing.B) {
	store := NewKVStore(1)
	store.Set("key", []byte("value"))
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			store.Get("key")
		}
	})
}
