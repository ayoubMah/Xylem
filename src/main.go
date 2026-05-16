package main

import (
	"fmt"
)

func main() {
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
		fmt.Printf("name deleted!")
	}
}
