package btree

import (
	"math/rand"
	"sort"
	"testing"
)

func TestEmptyTree(t *testing.T) {
	bt := New(2)
	if bt.Len() != 0 {
		t.Errorf("Len = %d, want 0", bt.Len())
	}
	if bt.Height() != 1 {
		t.Errorf("Height = %d, want 1", bt.Height())
	}
	if bt.Search(42) {
		t.Error("Search found a key in an empty tree")
	}
	if err := bt.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

func TestInsertSearchSmall(t *testing.T) {
	bt := New(2)
	keys := []int{10, 20, 5, 6, 12, 30, 7, 17}

	for _, k := range keys {
		if !bt.Insert(k) {
			t.Errorf("Insert(%d) reported the key was already present", k)
		}
		if err := bt.Validate(); err != nil {
			t.Fatalf("after Insert(%d): %v", k, err)
		}
	}

	if bt.Len() != len(keys) {
		t.Errorf("Len = %d, want %d", bt.Len(), len(keys))
	}
	for _, k := range keys {
		if !bt.Search(k) {
			t.Errorf("Search(%d) = false, want true", k)
		}
	}
	for _, k := range []int{0, 8, 11, 100, -3} {
		if bt.Search(k) {
			t.Errorf("Search(%d) = true, want false", k)
		}
	}
}

func TestDuplicatesRejected(t *testing.T) {
	bt := New(2)
	if !bt.Insert(1) {
		t.Fatal("first Insert(1) returned false")
	}
	if bt.Insert(1) {
		t.Error("second Insert(1) returned true, want false")
	}
	if bt.Len() != 1 {
		t.Errorf("Len = %d, want 1", bt.Len())
	}
	if err := bt.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

// TestRootSplitGrowsHeight pins the one place a B-Tree gets taller. With t=2 a
// node holds at most 3 keys, so the fourth insertion must split the root.
func TestRootSplitGrowsHeight(t *testing.T) {
	bt := New(2)
	for i := 1; i <= 3; i++ {
		bt.Insert(i)
		if got := bt.Height(); got != 1 {
			t.Fatalf("after %d insertions Height = %d, want 1", i, got)
		}
	}
	bt.Insert(4)
	if got := bt.Height(); got != 2 {
		t.Fatalf("after the root split Height = %d, want 2", got)
	}
	if err := bt.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

// TestAgainstReference is the correctness backstop: every operation is mirrored
// against a Go map, and the invariants are checked as the tree grows.
func TestAgainstReference(t *testing.T) {
	for _, degree := range []int{2, 3, 8, 64} {
		degree := degree
		t.Run("t="+itoa(degree), func(t *testing.T) {
			bt := New(degree)
			ref := make(map[int]bool)
			rng := rand.New(rand.NewSource(42))

			for i := 0; i < 5000; i++ {
				k := rng.Intn(2000) // deliberately dense, so duplicates occur
				inserted := bt.Insert(k)
				if inserted == ref[k] {
					t.Fatalf("Insert(%d) = %v but reference already had it = %v",
						k, inserted, ref[k])
				}
				ref[k] = true

				if i%250 == 0 {
					if err := bt.Validate(); err != nil {
						t.Fatalf("after %d insertions: %v", i, err)
					}
				}
			}

			if err := bt.Validate(); err != nil {
				t.Fatalf("final Validate: %v", err)
			}
			if bt.Len() != len(ref) {
				t.Fatalf("Len = %d, want %d", bt.Len(), len(ref))
			}
			for k := 0; k < 2000; k++ {
				if got := bt.Search(k); got != ref[k] {
					t.Fatalf("Search(%d) = %v, want %v", k, got, ref[k])
				}
			}
		})
	}
}

// TestKeysSorted checks the capability Bitcask cannot provide at any price:
// keys come back in order.
func TestKeysSorted(t *testing.T) {
	bt := New(2)
	rng := rand.New(rand.NewSource(7))
	want := make([]int, 0, 1000)
	for len(want) < 1000 {
		k := rng.Intn(1 << 20)
		if bt.Insert(k) {
			want = append(want, k)
		}
	}
	sort.Ints(want)

	got := bt.Keys()
	if len(got) != len(want) {
		t.Fatalf("Keys returned %d keys, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Keys()[%d] = %d, want %d", i, got[i], want[i])
		}
	}
}

// TestSequentialDoesNotDegenerate is the point of the whole structure.
//
// A naive binary search tree fed ascending keys degenerates into a linked list
// of height n. A B-Tree cannot: it grows only at the root, which lifts every
// leaf at once, so ascending input produces the same balanced shape as random
// input.
func TestSequentialDoesNotDegenerate(t *testing.T) {
	const n = 10000
	bt := New(2)
	for i := 0; i < n; i++ {
		bt.Insert(i)
	}
	if err := bt.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	// log_2(10000) is about 13.3; a degenerate tree would be 10000 deep.
	if h := bt.Height(); h > 20 {
		t.Errorf("Height = %d after %d ascending insertions -- tree degenerated", h, n)
	}
	for i := 0; i < n; i++ {
		if !bt.Search(i) {
			t.Fatalf("Search(%d) = false after sequential insert", i)
		}
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
