package lsm

import (
	"bytes"
	"fmt"
	"math/rand"
	"sort"
	"testing"
)

// TestMemTableAgainstMap is the workhorse: 5,000 randomised operations mirrored
// against a reference map, the same technique used for the Phase 3 B-Tree. The
// reference is obviously correct and slow; the structure under test is fast and
// only probably correct. Agreement over thousands of ops is the evidence.
func TestMemTableAgainstMap(t *testing.T) {
	const ops = 5000
	const keySpace = 400 // small enough that overwrites and deletes collide often

	m := NewMemTable(1)
	ref := map[string][]byte{} // live keys only
	dead := map[string]bool{}  // keys we have tombstoned

	rng := rand.New(rand.NewSource(42))

	for i := 0; i < ops; i++ {
		key := []byte(fmt.Sprintf("key%03d", rng.Intn(keySpace)))

		switch rng.Intn(10) {
		case 0, 1: // delete
			m.Delete(key)
			delete(ref, string(key))
			dead[string(key)] = true

		default: // set
			val := []byte(fmt.Sprintf("val-%d", i))
			m.Set(key, val)
			ref[string(key)] = val
			delete(dead, string(key))
		}

		// Spot-check a random key every iteration, live or not.
		probe := []byte(fmt.Sprintf("key%03d", rng.Intn(keySpace)))
		got, found, tomb := m.Get(probe)

		if want, live := ref[string(probe)]; live {
			if !found || tomb {
				t.Fatalf("op %d: key %q should be live, got found=%v tomb=%v", i, probe, found, tomb)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("op %d: key %q = %q, want %q", i, probe, got, want)
			}
		} else if dead[string(probe)] {
			if !found || !tomb {
				t.Fatalf("op %d: key %q should be tombstoned, got found=%v tomb=%v", i, probe, found, tomb)
			}
		} else {
			if found {
				t.Fatalf("op %d: key %q should be absent, got found=true", i, probe)
			}
		}
	}
}

// TestScanIsSorted proves requirement (1): the flush path gets keys in order
// without sorting anything.
func TestScanIsSorted(t *testing.T) {
	m := NewMemTable(7)

	// Insert in deliberately scrambled order.
	rng := rand.New(rand.NewSource(99))
	keys := make([]string, 500)
	for i := range keys {
		keys[i] = fmt.Sprintf("k%04d", i)
	}
	rng.Shuffle(len(keys), func(i, j int) { keys[i], keys[j] = keys[j], keys[i] })

	for _, k := range keys {
		m.Set([]byte(k), []byte("v"))
	}

	entries := m.Scan()
	if len(entries) != len(keys) {
		t.Fatalf("Scan returned %d entries, want %d", len(entries), len(keys))
	}

	for i := 1; i < len(entries); i++ {
		if bytes.Compare(entries[i-1].Key, entries[i].Key) >= 0 {
			t.Fatalf("Scan not sorted at %d: %q >= %q",
				i, entries[i-1].Key, entries[i].Key)
		}
	}

	// And it really is the same set we put in.
	sort.Strings(keys)
	for i, k := range keys {
		if string(entries[i].Key) != k {
			t.Fatalf("entry %d = %q, want %q", i, entries[i].Key, k)
		}
	}
}

// TestOverwriteDoesNotGrow proves requirement (2). This is the property that
// makes a memtable able to absorb a write-heavy workload over a hot key set
// without ever touching the disk.
func TestOverwriteDoesNotGrow(t *testing.T) {
	m := NewMemTable(3)

	for i := 0; i < 1000; i++ {
		m.Set([]byte("hot"), []byte(fmt.Sprintf("value-%04d", i)))
	}

	if got := m.Len(); got != 1 {
		t.Fatalf("Len after 1000 overwrites = %d, want 1", got)
	}

	// size tracks the CURRENT value, not the sum of all values ever written.
	wantSize := len("hot") + len("value-0999")
	if got := m.Size(); got != wantSize {
		t.Fatalf("Size = %d, want %d", got, wantSize)
	}

	v, found, tomb := m.Get([]byte("hot"))
	if !found || tomb || string(v) != "value-0999" {
		t.Fatalf("Get = (%q, %v, %v), want last value", v, found, tomb)
	}
}

// TestTombstoneIsNotAbsence is the distinction the read path depends on. If
// these two collapsed into one answer, a deleted key would be resurrected by
// whatever an older SSTable happens to hold.
func TestTombstoneIsNotAbsence(t *testing.T) {
	m := NewMemTable(5)
	m.Set([]byte("gone"), []byte("value"))
	m.Delete([]byte("gone"))

	if _, found, tomb := m.Get([]byte("gone")); !found || !tomb {
		t.Fatalf("deleted key: found=%v tomb=%v, want true/true", found, tomb)
	}
	if _, found, tomb := m.Get([]byte("never-existed")); found || tomb {
		t.Fatalf("absent key: found=%v tomb=%v, want false/false", found, tomb)
	}
}

// TestDeleteOfAbsentKeyStillWrites documents the counter-intuitive half: a
// delete can only ever ADD data. Same effect Phase 2 measured on the log.
func TestDeleteOfAbsentKeyStillWrites(t *testing.T) {
	m := NewMemTable(11)
	m.Delete([]byte("was-never-here"))

	if got := m.Len(); got != 1 {
		t.Fatalf("Len after deleting an absent key = %d, want 1 (a tombstone is a write)", got)
	}
	if got := m.Size(); got == 0 {
		t.Fatal("Size = 0 after a delete; the tombstone should cost its key bytes")
	}
}

// TestHeightIsOrderIndependent is the skiplist's answer to the Phase 3 finding
// that a naive BST fed ascending keys degenerates to a linked list of height
// 100,000. The coin does not know the key, so insertion order cannot hurt it.
func TestHeightIsOrderIndependent(t *testing.T) {
	const n = 20000

	ascending := NewMemTable(1)
	for i := 0; i < n; i++ {
		ascending.Set([]byte(fmt.Sprintf("k%08d", i)), []byte("v"))
	}

	shuffled := NewMemTable(1)
	order := rng(n, 1234)
	for _, i := range order {
		shuffled.Set([]byte(fmt.Sprintf("k%08d", i)), []byte("v"))
	}

	ah, sh := ascending.Height(), shuffled.Height()
	t.Logf("height: ascending=%d shuffled=%d (n=%d, log_4(n)=%.1f)",
		ah, sh, n, mathLog4(n))

	if ah != sh {
		t.Logf("note: heights differ by %d level(s) -- expected, the tower is random", ah-sh)
	}
	// The real assertion: neither degenerates. A linked list would be height 1
	// with n nodes to walk; a broken promotion would peg at maxHeight.
	for name, h := range map[string]int{"ascending": ah, "shuffled": sh} {
		if h < 6 || h > maxHeight {
			t.Fatalf("%s height = %d, want a sane tower between 6 and %d", name, h, maxHeight)
		}
	}
}

func rng(n int, seed int64) []int {
	r := rand.New(rand.NewSource(seed))
	out := make([]int, n)
	for i := range out {
		out[i] = i
	}
	r.Shuffle(n, func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out
}

func mathLog4(n int) float64 {
	c := 0.0
	for x := float64(n); x > 1; x /= 4 {
		c++
	}
	return c
}

// ---------------------------------------------------------------------------
// Benchmarks -- the memtable half of "LSM writes faster than a B-Tree".
// ---------------------------------------------------------------------------

func BenchmarkMemTableSetSequential(b *testing.B) {
	m := NewMemTable(1)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.Set([]byte(fmt.Sprintf("k%08d", i)), []byte("value"))
	}
}

func BenchmarkMemTableSetRandom(b *testing.B) {
	m := NewMemTable(1)
	r := rand.New(rand.NewSource(7))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.Set([]byte(fmt.Sprintf("k%08d", r.Intn(1<<20))), []byte("value"))
	}
}

func BenchmarkMemTableSetOverwrite(b *testing.B) {
	m := NewMemTable(1)
	key := []byte("hot-key")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.Set(key, []byte("value"))
	}
}

func BenchmarkMemTableGet(b *testing.B) {
	const n = 100000
	m := NewMemTable(1)
	for i := 0; i < n; i++ {
		m.Set([]byte(fmt.Sprintf("k%08d", i)), []byte("value"))
	}
	r := rand.New(rand.NewSource(7))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.Get([]byte(fmt.Sprintf("k%08d", r.Intn(n))))
	}
}
