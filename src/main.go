package main

import (
	"fmt"
	"time"
)

func main() {
	// stage1
	// store := NewKVStore(0)
	//
	// store.Set("name", []byte("Aub"))
	// store.Set("lang", []byte("Go"))
	//
	// if val, ok := store.Get("name"); ok {
	// 	fmt.Printf("name: %s\n", val)
	// }
	//
	// if val, ok := store.Get("lang"); ok {
	// 	fmt.Printf("lang: %s\n", val)
	// }
	//
	// store.Delete("name")
	// if _, ok := store.Get("name"); !ok {
	// 	fmt.Printf("name deleted!\n")
	// }

	// test the measureGrowth
	//measureGrowth()
	//
	// test the Traffic
	simulateTraffic()
}

func measureGrowth() {
	store := NewKVStore(0) // change it with make(map[string][]byte, 10000) and test again and change the line 42 to 43

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

func simulateTraffic() {
	store := NewKVStore(0)
	store.Set("status", []byte("running"))

	// 1 Writer trying to update the map
	go func() {
		for i := 0; i < 1000; i++ {
			store.Set("status", []byte("updating"))
		}
	}()

	// 10 Readers trying to read the map at the same time
	for i := 0; i < 10; i++ {
		go func() {
			for j := 0; j < 1000; j++ {
				store.Get("status")
			}
		}()
	}

	// Give the goroutines a second to finish their chaos
	time.Sleep(1 * time.Second)
	fmt.Println("Traffic simulation complete!")
}
