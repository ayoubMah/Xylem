package lsm

import (
	"fmt"
	"math/rand"
	"path/filepath"
	"testing"

	"github.com/ayoubMah/Xylem/bitcask"
)

// ---------------------------------------------------------------------------
// CROSS-ENGINE BENCHMARKS -- Phase 2 (Bitcask) vs Phase 4 (LSM)
// ---------------------------------------------------------------------------
//
// METHODOLOGY, same as the Phase 2 suite and for the same reason: this machine
// is a 15 W mobile CPU under Windows with real-time AV on the I/O path, so
// absolute nanosecond figures are indicative only. Every claim below is a RATIO
// between two engines measured IN THE SAME PROCESS, in the same run, against
// the same workload. Ratios survive a noisy host; absolute numbers do not.
//
// The comparison is Bitcask vs LSM rather than B-Tree vs LSM, deliberately. The
// Phase 3 B-Tree is in-memory and int-keyed, so benchmarking it against a
// persistent []byte-keyed engine would measure the difference between RAM and
// disk, not between the two data structures. Bitcask and the LSM are the honest
// pair: both persistent, both crash-safe, both byte-keyed, differing only in
// where a write goes.

const benchValueSize = 100

func benchValue() []byte {
	v := make([]byte, benchValueSize)
	for i := range v {
		v[i] = byte('a' + i%26)
	}
	return v
}

// ---------------------------------------------------------------------------
// WRITE PATH -- the LSM's whole reason to exist
// ---------------------------------------------------------------------------

// BenchmarkWrite_Bitcask: every Set is one append to the log plus a keydir
// update. Already fast -- this is the number the LSM has to beat.
func BenchmarkWrite_Bitcask(b *testing.B) {
	s, err := bitcask.Open(filepath.Join(b.TempDir(), "bc.log"), &bitcask.Options{})
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()

	val := benchValue()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := s.Set([]byte(fmt.Sprintf("k%08d", i)), val); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkWrite_LSM: every Set is one WAL append plus a skiplist insert. The
// SSTable write happens once per flush, amortised over an entire memtable.
func BenchmarkWrite_LSM(b *testing.B) {
	db, err := Open(b.TempDir(), &Options{MemTableSize: DefaultMemTableSize})
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()

	val := benchValue()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := db.Set([]byte(fmt.Sprintf("k%08d", i)), val); err != nil {
			b.Fatal(err)
		}
	}
}

// The overwrite workload is where the two engines diverge most sharply, and it
// is the case the LSM is actually designed for.
//
//	Bitcask: every overwrite APPENDS a new record. The log grows without bound
//	         and space amplification tracks the overwrite factor exactly, as
//	         Phase 2 measured.
//	LSM:     an overwrite inside one memtable generation is absorbed IN PLACE.
//	         Nothing reaches the disk at all until the flush, and the flush
//	         writes one entry, not N.
func BenchmarkOverwrite_Bitcask(b *testing.B) {
	s, err := bitcask.Open(filepath.Join(b.TempDir(), "bc.log"), &bitcask.Options{})
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()

	val := benchValue()
	r := rand.New(rand.NewSource(1))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// A small hot key set: the same 1,000 keys rewritten over and over.
		if err := s.Set([]byte(fmt.Sprintf("k%04d", r.Intn(1000))), val); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkOverwrite_LSM(b *testing.B) {
	db, err := Open(b.TempDir(), &Options{MemTableSize: DefaultMemTableSize})
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()

	val := benchValue()
	r := rand.New(rand.NewSource(1))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := db.Set([]byte(fmt.Sprintf("k%04d", r.Intn(1000))), val); err != nil {
			b.Fatal(err)
		}
	}
}

// The fsync dial, on both engines. Phase 2 measured 96.4x for Bitcask. The LSM
// pays it on a WAL append, which is the same shape of write -- so the ratio
// should be similar, and the interesting question is whether the LSM's extra
// in-memory work is visible underneath a cost that large. It should not be.
func BenchmarkWriteSync_Bitcask(b *testing.B) {
	s, err := bitcask.Open(filepath.Join(b.TempDir(), "bc.log"), &bitcask.Options{SyncOnWrite: true})
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()

	val := benchValue()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := s.Set([]byte(fmt.Sprintf("k%08d", i)), val); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkWriteSync_LSM(b *testing.B) {
	db, err := Open(b.TempDir(), &Options{MemTableSize: DefaultMemTableSize, SyncOnWrite: true})
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()

	val := benchValue()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := db.Set([]byte(fmt.Sprintf("k%08d", i)), val); err != nil {
			b.Fatal(err)
		}
	}
}

// ---------------------------------------------------------------------------
// READ PATH -- where the LSM pays the bill
// ---------------------------------------------------------------------------

const readCorpus = 50000

// BenchmarkRead_Bitcask: one map lookup, one seek. Bounded and predictable --
// the property the whole Bitcask design is built to guarantee.
func BenchmarkRead_Bitcask(b *testing.B) {
	s, err := bitcask.Open(filepath.Join(b.TempDir(), "bc.log"), &bitcask.Options{})
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()

	val := benchValue()
	for i := 0; i < readCorpus; i++ {
		s.Set([]byte(fmt.Sprintf("k%08d", i)), val)
	}

	r := rand.New(rand.NewSource(7))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.Get([]byte(fmt.Sprintf("k%08d", r.Intn(readCorpus)))); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkRead_LSM: memtable first, then a binary search plus one seek per
// SSTable until the key is found.
func BenchmarkRead_LSM(b *testing.B) {
	db, err := Open(b.TempDir(), &Options{MemTableSize: 1 << 20})
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()

	val := benchValue()
	for i := 0; i < readCorpus; i++ {
		db.Set([]byte(fmt.Sprintf("k%08d", i)), val)
	}
	db.Flush()

	r := rand.New(rand.NewSource(7))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := db.Get([]byte(fmt.Sprintf("k%08d", r.Intn(readCorpus)))); err != nil {
			b.Fatal(err)
		}
	}
}

// ---------------------------------------------------------------------------
// READ AMPLIFICATION -- the LSM's characteristic cost, isolated
// ---------------------------------------------------------------------------
//
// A lookup for a key that does not exist is an LSM's worst case: it cannot stop
// early, so it must consult EVERY table. These benchmarks vary only the number
// of tables, so the slope of the result IS the read amplification.
//
// With bloom filters the slope should be close to flat, because each table is
// ruled out from memory without a disk read. That is the entire argument for
// spending ~1 byte of RAM per key.

func benchmarkMissWithTables(b *testing.B, nTables int) {
	db, err := Open(b.TempDir(), &Options{MemTableSize: 1 << 30}) // never auto-flush
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()

	val := benchValue()
	const perTable = 2000
	for t := 0; t < nTables; t++ {
		for i := 0; i < perTable; i++ {
			db.Set([]byte(fmt.Sprintf("t%03d-k%06d", t, i)), val)
		}
		if err := db.Flush(); err != nil {
			b.Fatal(err)
		}
	}
	if db.Tables() != nTables {
		b.Fatalf("Tables = %d, want %d", db.Tables(), nTables)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// A key that exists in no table at all.
		db.Get([]byte(fmt.Sprintf("absent-%08d", i)))
	}
}

func BenchmarkReadMiss_1Table(b *testing.B)   { benchmarkMissWithTables(b, 1) }
func BenchmarkReadMiss_4Tables(b *testing.B)  { benchmarkMissWithTables(b, 4) }
func BenchmarkReadMiss_16Tables(b *testing.B) { benchmarkMissWithTables(b, 16) }
func BenchmarkReadMiss_32Tables(b *testing.B) { benchmarkMissWithTables(b, 32) }

// BenchmarkReadMiss_NoBloom_16Tables is the control. It strips the filters off
// an otherwise identical database, so the difference against
// BenchmarkReadMiss_16Tables is exactly what the bloom filters are worth.
func BenchmarkReadMiss_NoBloom_16Tables(b *testing.B) {
	db, err := Open(b.TempDir(), &Options{MemTableSize: 1 << 30})
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()

	val := benchValue()
	for t := 0; t < 16; t++ {
		for i := 0; i < 2000; i++ {
			db.Set([]byte(fmt.Sprintf("t%03d-k%06d", t, i)), val)
		}
		db.Flush()
	}

	// Disable every filter: m=0 makes mayContain return true unconditionally,
	// which is the "no filter" behaviour by construction.
	db.mu.Lock()
	for _, tbl := range db.tables {
		tbl.bloom = &bloomFilter{m: 0}
	}
	db.mu.Unlock()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		db.Get([]byte(fmt.Sprintf("absent-%08d", i)))
	}
}

// ---------------------------------------------------------------------------
// COMPACTION -- the deferred bill, measured
// ---------------------------------------------------------------------------

func BenchmarkCompaction(b *testing.B) {
	val := benchValue()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		db, err := Open(b.TempDir(), &Options{MemTableSize: 1 << 30})
		if err != nil {
			b.Fatal(err)
		}
		// 8 tables, 2,000 keys each, every key written 8 times.
		for t := 0; t < 8; t++ {
			for k := 0; k < 2000; k++ {
				db.Set([]byte(fmt.Sprintf("k%06d", k)), val)
			}
			db.Flush()
		}
		b.StartTimer()

		if err := db.Compact(); err != nil {
			b.Fatal(err)
		}

		b.StopTimer()
		db.Close()
		b.StartTimer()
	}
}
