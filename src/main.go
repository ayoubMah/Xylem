package main

import (
	"fmt"
	"time"
)

func main() {
	// stage1
	store := NewKVStore()

	store.Set("name", []byte("Aub"))
	store.Set("lang", []byte("Go"))

	if val, ok := store.Get("name"); ok {
		fmt.Printf("name: %s\n", val)
	}

	if val, ok := store.Get("lang"); ok {
		fmt.Printf("lang: %s\n", val)
	}

	store.Delete("name")
	if _, ok := store.Get("name"); !ok {
		fmt.Printf("name deleted!\n")
	}

	// test the measureGrowth
	measureGrowth()
}

func measureGrowth() {
	store := NewKVStore() // change it with make(map[string][]byte, 10000) and test again and change the line 42 to 43

	threshold := 2 * time.Microsecond

	for i := range 10_000 {
		key := fmt.Sprintf("key-%d", i)
		value := []byte("v")

		start := time.Now()
		store.Set(key, value)
		// store[key] = value
		elapsed := time.Since(start)

		if elapsed > threshold {
			fmt.Printf("spike at key #%d: %v\n", i, elapsed)
		}
	}
}
