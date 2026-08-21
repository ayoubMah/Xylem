package bitcask

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// ---------------------------------------------------------------------------
// BENCHMARK METHODOLOGY
// ---------------------------------------------------------------------------
//
// Every number quoted in the thesis comes from this file. The parameters are
// fixed here, in code, so that a reader can reproduce them exactly:
//
//	key   = "key:%08d"   -> 12 bytes
//	value = benchValSize -> 100 bytes
//	record on disk = 20 (header) + 12 (key) + 100 (value) = 132 bytes
//
// Keys are pre-generated before the timer starts: formatting a string costs on
// the order of 100 ns and would otherwise be measured as if it were storage
// cost.
//
// Run with:
//
//	go test -bench=. -benchmem ./bitcask/...
const (
	benchValSize = 100
	benchKeyFmt  = "key:%08d"
)

func benchKeys(n int) [][]byte {
	keys := make([][]byte, n)
	for i := range keys {
		keys[i] = []byte(fmt.Sprintf(benchKeyFmt, i))
	}
	return keys
}

func benchValue() []byte {
	v := make([]byte, benchValSize)
	for i := range v {
		v[i] = byte('a' + i%26)
	}
	return v
}

func benchStore(b *testing.B, syncOnWrite bool) *Store {
	b.Helper()
	path := filepath.Join(b.TempDir(), "xylem.log")
	s, err := Open(path, &Options{SyncOnWrite: syncOnWrite})
	if err != nil {
		b.Fatalf("Open: %v", err)
	}
	b.Cleanup(func() { s.Close() })
	return s
}

// ---------------------------------------------------------------------------
// WRITE PATH -- the durability dial
// ---------------------------------------------------------------------------

// BenchmarkSet measures an append that returns once write(2) has handed the
// bytes to the OS page cache. Survives a process crash, NOT a power loss.
func BenchmarkSet(b *testing.B) {
	s := benchStore(b, false)
	keys := benchKeys(b.N)
	val := benchValue()
	b.SetBytes(HeaderSize + int64(len(keys[0])) + benchValSize)
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		if err := s.Set(keys[i], val); err != nil {
			b.Fatalf("Set: %v", err)
		}
	}
}

// BenchmarkSetSync measures the same append with an fsync before returning.
// The ratio BenchmarkSetSync/BenchmarkSet IS the price of power-loss
// durability, and is the headline number of the persistence chapter.
func BenchmarkSetSync(b *testing.B) {
	s := benchStore(b, true)
	keys := benchKeys(b.N)
	val := benchValue()
	b.SetBytes(HeaderSize + int64(len(keys[0])) + benchValSize)
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		if err := s.Set(keys[i], val); err != nil {
			b.Fatalf("Set: %v", err)
		}
	}
}

// BenchmarkSetOverwrite writes the SAME key repeatedly. Under an append-only
// design this costs exactly as much as writing distinct keys -- the difference
// shows up later as garbage for Merge to reclaim, not as write latency.
func BenchmarkSetOverwrite(b *testing.B) {
	s := benchStore(b, false)
	key := []byte("hot:key")
	val := benchValue()
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		if err := s.Set(key, val); err != nil {
			b.Fatalf("Set: %v", err)
		}
	}
}

// ---------------------------------------------------------------------------
// READ PATH -- one map lookup + one positional read
// ---------------------------------------------------------------------------

const benchReadCorpus = 10000

func benchLoadedStore(b *testing.B) (*Store, [][]byte) {
	b.Helper()
	s := benchStore(b, false)
	keys := benchKeys(benchReadCorpus)
	val := benchValue()
	for _, k := range keys {
		if err := s.Set(k, val); err != nil {
			b.Fatalf("seed Set: %v", err)
		}
	}
	return s, keys
}

// BenchmarkGet is the central claim of the Bitcask model: a read is O(1) in the
// size of the log, because the keydir already knows the offset.
func BenchmarkGet(b *testing.B) {
	s, keys := benchLoadedStore(b)
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		if _, err := s.Get(keys[i%len(keys)]); err != nil {
			b.Fatalf("Get: %v", err)
		}
	}
}

// BenchmarkGetParallel exercises the RWMutex: many readers, no writer. This is
// what the read-optimised locking inherited from Phase 1 is supposed to buy.
func BenchmarkGetParallel(b *testing.B) {
	s, keys := benchLoadedStore(b)
	b.ReportAllocs()
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			if _, err := s.Get(keys[i%len(keys)]); err != nil {
				b.Fatalf("Get: %v", err)
			}
			i++
		}
	})
}

// ---------------------------------------------------------------------------
// PHASE 1 BASELINE -- the same workload with no disk at all
// ---------------------------------------------------------------------------
//
// Phase 1 lives in `package main` and cannot be imported here, so its data
// structure is reproduced verbatim: map[string][]byte behind a sync.RWMutex.
// This is the comparison the Evaluation chapter rests on -- it isolates the
// cost of PERSISTENCE, because everything else about the two paths is
// identical.

type memStore struct {
	mu   sync.RWMutex
	data map[string][]byte
}

func newMemStore(capacity int) *memStore {
	return &memStore{data: make(map[string][]byte, capacity)}
}

func (s *memStore) Set(key string, value []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[key] = value
}

func (s *memStore) Get(key string) ([]byte, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.data[key]
	return v, ok
}

func BenchmarkMemSet(b *testing.B) {
	s := newMemStore(b.N)
	keys := benchKeys(b.N)
	val := benchValue()
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		s.Set(string(keys[i]), val)
	}
}

func BenchmarkMemGet(b *testing.B) {
	s := newMemStore(benchReadCorpus)
	keys := benchKeys(benchReadCorpus)
	val := benchValue()
	for _, k := range keys {
		s.Set(string(k), val)
	}
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		if _, ok := s.Get(string(keys[i%len(keys)])); !ok {
			b.Fatal("missing key")
		}
	}
}

// ---------------------------------------------------------------------------
// RECOVERY -- Open() replays the whole log, so cost is linear in log SIZE
// ---------------------------------------------------------------------------
//
// This is the structural weakness of the Bitcask model, and the benchmark that
// proves it: startup time is not a function of how much data is LIVE, but of
// how many records were ever written. Merge is what keeps it bounded.

func benchmarkOpen(b *testing.B, records int) {
	path := filepath.Join(b.TempDir(), "xylem.log")

	// Build the log once, outside the timer.
	s, err := Open(path, &Options{})
	if err != nil {
		b.Fatalf("Open: %v", err)
	}
	keys := benchKeys(records)
	val := benchValue()
	for _, k := range keys {
		if err := s.Set(k, val); err != nil {
			b.Fatalf("seed Set: %v", err)
		}
	}
	if err := s.Close(); err != nil {
		b.Fatalf("Close: %v", err)
	}

	fi, err := os.Stat(path)
	if err != nil {
		b.Fatalf("Stat: %v", err)
	}
	b.ReportMetric(float64(fi.Size())/(1<<20), "logMiB")
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		st, err := Open(path, &Options{})
		if err != nil {
			b.Fatalf("Open: %v", err)
		}
		if st.Len() != records {
			b.Fatalf("recovered %d keys, want %d", st.Len(), records)
		}
		if err := st.Close(); err != nil {
			b.Fatalf("Close: %v", err)
		}
	}
}

func BenchmarkOpen1k(b *testing.B)   { benchmarkOpen(b, 1000) }
func BenchmarkOpen10k(b *testing.B)  { benchmarkOpen(b, 10000) }
func BenchmarkOpen100k(b *testing.B) { benchmarkOpen(b, 100000) }

// ---------------------------------------------------------------------------
// MERGE -- throughput of compaction itself
// ---------------------------------------------------------------------------

func BenchmarkMerge(b *testing.B) {
	keys := benchKeys(1000)
	val := benchValue()

	for i := 0; i < b.N; i++ {
		b.StopTimer()
		path := filepath.Join(b.TempDir(), "xylem.log")
		s, err := Open(path, &Options{})
		if err != nil {
			b.Fatalf("Open: %v", err)
		}
		// 1000 live keys, each written 10 times => 90% of the log is garbage.
		for round := 0; round < 10; round++ {
			for _, k := range keys {
				if err := s.Set(k, val); err != nil {
					b.Fatalf("Set: %v", err)
				}
			}
		}
		b.StartTimer()

		if err := s.Merge(); err != nil {
			b.Fatalf("Merge: %v", err)
		}

		b.StopTimer()
		s.Close()
		b.StartTimer()
	}
}
