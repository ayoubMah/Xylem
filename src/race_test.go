package main

import (
	"sync"
	"testing"
)

func TestUnsafeRace(t *testing.T) {
	store := NewUnsafeKVStore()
	store.Set("status", []byte("ok"))

	var wg sync.WaitGroup

	// 1 writer
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			store.Set("status", []byte("updating"))
		}
	}()

	// 10 readers
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 1000; j++ {
				store.Get("status")
			}
		}()
	}

	wg.Wait()
}
