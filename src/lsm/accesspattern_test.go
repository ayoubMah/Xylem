package lsm

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
)

// ---------------------------------------------------------------------------
// THE ACCESS PATTERN, ISOLATED FROM THE ENGINE
// ---------------------------------------------------------------------------
//
// The cross-engine benchmarks compare two whole storage engines, which means
// every number they produce mixes the access pattern with the bookkeeping
// around it. This file strips the bookkeeping away and measures ONLY the thing
// the LSM claims to fix, at the syscall level -- the same role rawio_test.go
// played in Phase 2, and for the same reason: without a floor, no result can be
// attributed to a design rather than to the platform.
//
// The claim under test is the central one in the whole LSM literature:
//
//	A B-Tree updates a record IN PLACE, at whatever offset the key hashes to.
//	That is a RANDOM WRITE. An LSM buffers in memory and dumps sorted, so its
//	only disk write is a SEQUENTIAL APPEND. Sequential writes are cheaper,
//	therefore the LSM wins on the write path.
//
// Every clause of that is testable here with nothing but a file and an offset.
//
// NOTE ON WHAT THIS IS NOT: this is a stand-in for a B-Tree's write pattern,
// not a B-Tree. It has no nodes, no splits, no rebalancing -- so it is a LOWER
// BOUND on what an on-disk B-Tree's write would cost, and the real structure
// can only be slower. That makes it the right shape of evidence: if the log
// already beats the lower bound, it beats the real thing.

const (
	slotSize = 128    // one "record slot", like a B-Tree entry
	numSlots = 200000 // ~25 MiB file: bigger than CPU cache, small enough to be quick
)

// prepareSlotFile creates a pre-sized file of fixed slots.
func prepareSlotFile(b *testing.B) *os.File {
	b.Helper()
	path := filepath.Join(b.TempDir(), "slots.dat")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		b.Fatal(err)
	}
	if err := f.Truncate(slotSize * numSlots); err != nil {
		b.Fatal(err)
	}
	if err := f.Sync(); err != nil {
		b.Fatal(err)
	}
	return f
}

// BenchmarkPattern_RandomInPlace is the B-Tree-shaped write: seek to an
// arbitrary offset and overwrite what is there.
func BenchmarkPattern_RandomInPlace(b *testing.B) {
	f := prepareSlotFile(b)
	defer f.Close()

	buf := make([]byte, slotSize)
	r := rand.New(rand.NewSource(1))

	b.SetBytes(slotSize)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		off := int64(r.Intn(numSlots)) * slotSize
		if _, err := f.WriteAt(buf, off); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkPattern_SequentialAppend is the log-shaped write: always at the end.
func BenchmarkPattern_SequentialAppend(b *testing.B) {
	path := filepath.Join(b.TempDir(), "log.dat")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		b.Fatal(err)
	}
	defer f.Close()

	buf := make([]byte, slotSize)
	var off int64

	b.SetBytes(slotSize)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := f.WriteAt(buf, off); err != nil {
			b.Fatal(err)
		}
		off += slotSize
	}
}

// The same two patterns with fsync after every write.
//
// This is where the difference should actually appear, and the reason is worth
// stating: WITHOUT fsync, both writes land in the OS page cache, and the page
// cache does not care where in the file a page lives -- so both look identical
// and the "sequential is faster" claim looks false. The cost of randomness is
// paid when pages are written BACK to the device: sequential writes dirty
// adjacent pages that the kernel can coalesce into one large I/O, random writes
// dirty scattered pages that cannot be merged.
//
// So the page cache is not just noise in this measurement -- it is the thing
// that HIDES the effect, and fsync is what removes the hiding place.
func BenchmarkPattern_RandomInPlaceSync(b *testing.B) {
	f := prepareSlotFile(b)
	defer f.Close()

	buf := make([]byte, slotSize)
	r := rand.New(rand.NewSource(1))

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		off := int64(r.Intn(numSlots)) * slotSize
		if _, err := f.WriteAt(buf, off); err != nil {
			b.Fatal(err)
		}
		if err := f.Sync(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkPattern_SequentialAppendSync(b *testing.B) {
	path := filepath.Join(b.TempDir(), "log.dat")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		b.Fatal(err)
	}
	defer f.Close()

	buf := make([]byte, slotSize)
	var off int64

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := f.WriteAt(buf, off); err != nil {
			b.Fatal(err)
		}
		if err := f.Sync(); err != nil {
			b.Fatal(err)
		}
		off += slotSize
	}
}

// BenchmarkPattern_BatchedSequential is the LSM's actual write pattern, and the
// point of the whole file.
//
// An LSM does not fsync per write. It absorbs a memtable's worth of writes in
// RAM and then flushes them as ONE sequential run with ONE fsync at the end.
// This measures the per-record cost of that: batch the writes, sync once.
//
// If the LSM's argument is right, this should be dramatically cheaper per
// record than either per-write-sync pattern above -- not because appending is
// magic, but because the durability cost is amortised across the whole batch.
func BenchmarkPattern_BatchedSequential(b *testing.B) {
	const batch = 1000

	path := filepath.Join(b.TempDir(), "batch.dat")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		b.Fatal(err)
	}
	defer f.Close()

	buf := make([]byte, slotSize)
	var off int64

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := f.WriteAt(buf, off); err != nil {
			b.Fatal(err)
		}
		off += slotSize
		if (i+1)%batch == 0 {
			if err := f.Sync(); err != nil {
				b.Fatal(err)
			}
		}
	}
	f.Sync()
}

// ---------------------------------------------------------------------------
// SPACE: what an overwrite actually costs each engine on disk
// ---------------------------------------------------------------------------

// TestOverwriteSpaceAmplification measures the LSM's answer to the Phase 2
// finding that Bitcask's space amplification equals the overwrite factor
// exactly. The memtable absorbs overwrites within one generation, so the
// SSTable holds ONE entry per key regardless of how many times it was written.
func TestOverwriteSpaceAmplification(t *testing.T) {
	const keys = 500
	value := make([]byte, 100)

	for _, overwrites := range []int{1, 2, 5, 10, 50} {
		dir := t.TempDir()
		db, err := Open(dir, &Options{MemTableSize: 1 << 30}) // one generation
		if err != nil {
			t.Fatal(err)
		}

		for gen := 0; gen < overwrites; gen++ {
			for i := 0; i < keys; i++ {
				if err := db.Set([]byte(fmt.Sprintf("k%06d", i)), value); err != nil {
					t.Fatal(err)
				}
			}
		}

		walBytes := db.wal.Size()
		if err := db.Flush(); err != nil {
			t.Fatal(err)
		}
		sstBytes := dirSize(t, dir)
		db.Close()

		t.Logf("overwrite x%-3d -> WAL %8d B (grows with every write)  SSTable %7d B (one entry per key)  amp %.2fx",
			overwrites, walBytes, sstBytes, float64(sstBytes)/float64(keys*(6+1+100)))

		// The SSTable size must be independent of the overwrite factor. That is
		// the claim; anything else means the memtable is not absorbing.
		if sstBytes > keys*400 {
			t.Fatalf("SSTable is %d B after %dx overwrites -- the memtable did not absorb them",
				sstBytes, overwrites)
		}
	}
}
