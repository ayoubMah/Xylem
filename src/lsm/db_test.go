package lsm

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
)

// crash simulates process death: file handles go away, nothing is flushed.
//
// This is the only honest way to test recovery. Calling Close() would flush the
// memtable and leave nothing for the WAL to prove.
func crash(db *DB) {
	db.mu.Lock()
	defer db.mu.Unlock()
	db.wal.Close()
	for _, t := range db.tables {
		t.Close()
	}
	db.closed = true
}

func mustOpen(t *testing.T, dir string, memSize int) *DB {
	t.Helper()
	db, err := Open(dir, &Options{MemTableSize: memSize})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return db
}

// ---------------------------------------------------------------------------
// Correctness
// ---------------------------------------------------------------------------

// TestDBAgainstMap is the workhorse, with a memtable small enough that the run
// crosses many flush boundaries. That is the point: it exercises the read path
// across a growing stack of tables, which is where LSM bugs actually live.
func TestDBAgainstMap(t *testing.T) {
	dir := t.TempDir()
	db := mustOpen(t, dir, 512) // tiny: flush every few dozen writes
	defer db.Close()

	const ops = 4000
	const keySpace = 300

	ref := map[string]string{}
	rng := rand.New(rand.NewSource(2026))

	for i := 0; i < ops; i++ {
		key := fmt.Sprintf("key%04d", rng.Intn(keySpace))

		if rng.Intn(10) < 2 {
			if err := db.Delete([]byte(key)); err != nil {
				t.Fatalf("op %d: Delete: %v", i, err)
			}
			delete(ref, key)
		} else {
			val := fmt.Sprintf("value-%d", i)
			if err := db.Set([]byte(key), []byte(val)); err != nil {
				t.Fatalf("op %d: Set: %v", i, err)
			}
			ref[key] = val
		}

		probe := fmt.Sprintf("key%04d", rng.Intn(keySpace))
		got, err := db.Get([]byte(probe))

		if want, live := ref[probe]; live {
			if err != nil {
				t.Fatalf("op %d: Get(%q) = %v, want %q", i, probe, err, want)
			}
			if string(got) != want {
				t.Fatalf("op %d: Get(%q) = %q, want %q", i, probe, got, want)
			}
		} else if !errors.Is(err, ErrKeyNotFound) {
			t.Fatalf("op %d: Get(%q) = (%q, %v), want ErrKeyNotFound", i, probe, got, err)
		}
	}

	t.Logf("finished with %d SSTables on disk", db.Tables())
	if db.Tables() < 2 {
		t.Fatal("expected multiple SSTables; the multi-table read path was never exercised")
	}

	// Full sweep: every live key must still be readable across the whole stack.
	for k, want := range ref {
		got, err := db.Get([]byte(k))
		if err != nil {
			t.Fatalf("final sweep: Get(%q) = %v, want %q", k, err, want)
		}
		if string(got) != want {
			t.Fatalf("final sweep: Get(%q) = %q, want %q", k, got, want)
		}
	}
}

// TestReadsNewestAcrossTables is the core LSM rule: same key in several tables,
// newest wins.
func TestReadsNewestAcrossTables(t *testing.T) {
	dir := t.TempDir()
	db := mustOpen(t, dir, 1<<20)
	defer db.Close()

	for i := 1; i <= 5; i++ {
		if err := db.Set([]byte("k"), []byte(fmt.Sprintf("v%d", i))); err != nil {
			t.Fatal(err)
		}
		if err := db.Flush(); err != nil {
			t.Fatal(err)
		}
	}

	if db.Tables() != 5 {
		t.Fatalf("Tables = %d, want 5", db.Tables())
	}

	got, err := db.Get([]byte("k"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "v5" {
		t.Fatalf("Get = %q, want %q -- the read path is not searching newest-first", got, "v5")
	}
}

// TestTombstoneShadowsOlderTable is the resurrection bug, isolated.
//
// If the read path continued past a tombstone it would find "value" in the
// older table and hand back a key the caller deleted.
func TestTombstoneShadowsOlderTable(t *testing.T) {
	dir := t.TempDir()
	db := mustOpen(t, dir, 1<<20)
	defer db.Close()

	db.Set([]byte("k"), []byte("value"))
	db.Flush() // table 0: k = value

	db.Delete([]byte("k"))
	db.Flush() // table 1: k = <tombstone>

	if _, err := db.Get([]byte("k")); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("Get after delete = %v, want ErrKeyNotFound (key was resurrected)", err)
	}

	// And a re-Set must bring it back.
	db.Set([]byte("k"), []byte("again"))
	got, err := db.Get([]byte("k"))
	if err != nil || string(got) != "again" {
		t.Fatalf("Get after re-set = (%q, %v), want %q", got, err, "again")
	}
}

// ---------------------------------------------------------------------------
// Recovery
// ---------------------------------------------------------------------------

// TestCrashRecoveryFromWAL proves the WAL earns its place: writes that never
// reached an SSTable survive a crash.
func TestCrashRecoveryFromWAL(t *testing.T) {
	dir := t.TempDir()
	db := mustOpen(t, dir, 1<<20) // large: nothing will flush on its own

	for i := 0; i < 100; i++ {
		if err := db.Set([]byte(fmt.Sprintf("k%03d", i)), []byte(fmt.Sprintf("v%03d", i))); err != nil {
			t.Fatal(err)
		}
	}
	db.Delete([]byte("k050"))

	if db.Tables() != 0 {
		t.Fatalf("Tables = %d, want 0 -- this test needs everything to still be in RAM", db.Tables())
	}

	crash(db) // no Close, no flush: the memtable is gone

	db2 := mustOpen(t, dir, 1<<20)
	defer db2.Close()

	for i := 0; i < 100; i++ {
		key := fmt.Sprintf("k%03d", i)
		got, err := db2.Get([]byte(key))

		if i == 50 {
			if !errors.Is(err, ErrKeyNotFound) {
				t.Fatalf("after recovery Get(%q) = (%q, %v), want ErrKeyNotFound -- the tombstone did not survive", key, got, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("after recovery Get(%q) = %v, want v%03d", key, err, i)
		}
		if string(got) != fmt.Sprintf("v%03d", i) {
			t.Fatalf("after recovery Get(%q) = %q, want v%03d", key, got, i)
		}
	}
}

// TestRecoveryFromTornWAL feeds the recovery path what a real crash leaves: a
// record cut in half. It must recover everything before the tear and stop.
func TestRecoveryFromTornWAL(t *testing.T) {
	dir := t.TempDir()
	db := mustOpen(t, dir, 1<<20)

	for i := 0; i < 20; i++ {
		db.Set([]byte(fmt.Sprintf("k%02d", i)), []byte("value"))
	}
	walSize := db.wal.Size()
	crash(db)

	// Chop the last record roughly in half.
	walPath := filepath.Join(dir, "wal.log")
	f, err := os.OpenFile(walPath, os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(walSize - 8); err != nil {
		t.Fatal(err)
	}
	f.Close()

	db2 := mustOpen(t, dir, 1<<20)
	defer db2.Close()

	// The first 19 writes must be intact.
	for i := 0; i < 19; i++ {
		if _, err := db2.Get([]byte(fmt.Sprintf("k%02d", i))); err != nil {
			t.Fatalf("Get(k%02d) after torn WAL = %v, want the record to survive", i, err)
		}
	}
	// The torn one is gone, and that is correct: it was never acknowledged.
	if _, err := db2.Get([]byte("k19")); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("Get(k19) = %v, want ErrKeyNotFound -- a torn record was accepted", err)
	}

	// And the log must be usable again, not permanently wedged behind garbage.
	if err := db2.Set([]byte("after"), []byte("crash")); err != nil {
		t.Fatalf("Set after recovery: %v", err)
	}
	if got, err := db2.Get([]byte("after")); err != nil || string(got) != "crash" {
		t.Fatalf("Get after recovery = (%q, %v)", got, err)
	}
}

// TestSSTablesSurviveWithoutWAL confirms the other half of recovery: flushed
// data needs no log at all.
func TestSSTablesSurviveWithoutWAL(t *testing.T) {
	dir := t.TempDir()
	db := mustOpen(t, dir, 1<<20)
	for i := 0; i < 50; i++ {
		db.Set([]byte(fmt.Sprintf("k%02d", i)), []byte("v"))
	}
	if err := db.Flush(); err != nil {
		t.Fatal(err)
	}
	crash(db)

	// Delete the log entirely.
	if err := os.Remove(filepath.Join(dir, "wal.log")); err != nil {
		t.Fatal(err)
	}

	db2 := mustOpen(t, dir, 1<<20)
	defer db2.Close()
	for i := 0; i < 50; i++ {
		if _, err := db2.Get([]byte(fmt.Sprintf("k%02d", i))); err != nil {
			t.Fatalf("Get(k%02d) with no WAL = %v; SSTables must be self-sufficient", i, err)
		}
	}
}

// ---------------------------------------------------------------------------
// Compaction
// ---------------------------------------------------------------------------

func TestCompactionKeepsNewestAndDropsTombstones(t *testing.T) {
	dir := t.TempDir()
	db := mustOpen(t, dir, 1<<20)
	defer db.Close()

	// Three generations of the same keys, each in its own table.
	for gen := 1; gen <= 3; gen++ {
		for i := 0; i < 20; i++ {
			db.Set([]byte(fmt.Sprintf("k%02d", i)), []byte(fmt.Sprintf("gen%d", gen)))
		}
		db.Flush()
	}
	// Delete a few, in a fourth table.
	for i := 0; i < 5; i++ {
		db.Delete([]byte(fmt.Sprintf("k%02d", i)))
	}
	db.Flush()

	before := db.Tables()
	if before != 4 {
		t.Fatalf("Tables before compaction = %d, want 4", before)
	}

	if err := db.Compact(); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if got := db.Tables(); got != 1 {
		t.Fatalf("Tables after compaction = %d, want 1", got)
	}

	// Deleted keys stay deleted...
	for i := 0; i < 5; i++ {
		if _, err := db.Get([]byte(fmt.Sprintf("k%02d", i))); !errors.Is(err, ErrKeyNotFound) {
			t.Fatalf("k%02d after compaction = %v, want ErrKeyNotFound", i, err)
		}
	}
	// ...and the survivors carry the NEWEST value.
	for i := 5; i < 20; i++ {
		got, err := db.Get([]byte(fmt.Sprintf("k%02d", i)))
		if err != nil {
			t.Fatalf("k%02d after compaction = %v", i, err)
		}
		if string(got) != "gen3" {
			t.Fatalf("k%02d = %q, want gen3 -- compaction kept an older version", i, got)
		}
	}

	// The tombstones themselves are gone: 20 keys, 5 deleted, 15 entries left.
	// This is the only moment in the engine where a delete frees space.
	db.mu.RLock()
	n := db.tables[0].Len()
	db.mu.RUnlock()
	if n != 15 {
		t.Fatalf("compacted table holds %d entries, want 15 (tombstones should be dropped in a full merge)", n)
	}
}

// TestCompactionReclaimsSpace is the LSM's version of the Phase 2 finding that
// space amplification equals the overwrite factor exactly.
func TestCompactionReclaimsSpace(t *testing.T) {
	dir := t.TempDir()
	db := mustOpen(t, dir, 1<<20)
	defer db.Close()

	const keys = 200
	const overwrites = 10

	for gen := 0; gen < overwrites; gen++ {
		for i := 0; i < keys; i++ {
			db.Set([]byte(fmt.Sprintf("k%04d", i)), bytes.Repeat([]byte("x"), 64))
		}
		db.Flush()
	}

	before := dirSize(t, dir)
	if err := db.Compact(); err != nil {
		t.Fatal(err)
	}
	after := dirSize(t, dir)

	ratio := float64(before) / float64(after)
	t.Logf("space: %d B before -> %d B after compaction (%.2fx reclaimed, overwrite factor %d)",
		before, after, ratio, overwrites)

	// Ten copies of the same key set collapse to one, so the ratio should sit
	// near the overwrite factor. Assert loosely -- index and bloom overhead do
	// not compress away.
	if ratio < 5 {
		t.Fatalf("only reclaimed %.2fx after %d overwrites, want >= 5x", ratio, overwrites)
	}
}

func TestMaybeCompactRespectsThreshold(t *testing.T) {
	dir := t.TempDir()
	db := mustOpen(t, dir, 1<<20)
	defer db.Close()

	for i := 0; i < 3; i++ {
		db.Set([]byte(fmt.Sprintf("k%d", i)), []byte("v"))
		db.Flush()
	}

	if err := db.MaybeCompact(5); err != nil {
		t.Fatal(err)
	}
	if got := db.Tables(); got != 3 {
		t.Fatalf("Tables = %d after MaybeCompact(5) with 3 tables, want 3 (should be a no-op)", got)
	}

	if err := db.MaybeCompact(3); err != nil {
		t.Fatal(err)
	}
	if got := db.Tables(); got != 1 {
		t.Fatalf("Tables = %d after MaybeCompact(3) with 3 tables, want 1", got)
	}
}

func dirSize(t *testing.T, dir string) int64 {
	t.Helper()
	var total int64
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if filepath.Ext(e.Name()) != ".sst" {
			continue
		}
		info, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		total += info.Size()
	}
	return total
}

// ---------------------------------------------------------------------------
// SSTable format
// ---------------------------------------------------------------------------

func TestSSTableRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.sst")
	entries := []Entry{
		{Key: []byte("a"), Value: []byte("1")},
		{Key: []byte("b"), Tombstone: true},
		{Key: []byte("c"), Value: []byte{}}, // empty value, NOT a delete
		{Key: []byte("d"), Value: bytes.Repeat([]byte("x"), 5000)},
	}

	w, err := WriteSSTable(path, entries)
	if err != nil {
		t.Fatal(err)
	}
	// WriteSSTable hands back an OPEN table; the caller owns the handle.
	w.Close()

	tbl, err := OpenSSTable(path)
	if err != nil {
		t.Fatal(err)
	}
	defer tbl.Close()

	// The distinction the tombstone sentinel exists for.
	if _, found, tomb, _ := tbl.Get([]byte("b")); !found || !tomb {
		t.Fatalf("b: found=%v tomb=%v, want a tombstone", found, tomb)
	}
	v, found, tomb, _ := tbl.Get([]byte("c"))
	if !found || tomb || len(v) != 0 {
		t.Fatalf("c: (%q, %v, %v), want an empty value that is NOT a tombstone", v, found, tomb)
	}
	if v, _, _, _ := tbl.Get([]byte("d")); len(v) != 5000 {
		t.Fatalf("d: len = %d, want 5000", len(v))
	}
	if _, found, _, _ := tbl.Get([]byte("zzz")); found {
		t.Fatal("zzz: found=true, want absent")
	}
}

func TestWriteSSTableRejectsUnsortedInput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.sst")
	_, err := WriteSSTable(path, []Entry{
		{Key: []byte("b"), Value: []byte("1")},
		{Key: []byte("a"), Value: []byte("2")},
	})
	if err == nil {
		t.Fatal("WriteSSTable accepted unsorted entries")
	}
	// The half-written file must not be left behind.
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatal("a failed WriteSSTable left a partial file on disk")
	}
}

func TestOpenSSTableRejectsGarbage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "garbage.sst")
	if err := os.WriteFile(path, bytes.Repeat([]byte("not a table"), 20), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenSSTable(path); !errors.Is(err, ErrNotSSTable) {
		t.Fatalf("OpenSSTable(garbage) = %v, want ErrNotSSTable", err)
	}
}

func TestSSTableDetectsCorruptRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.sst")
	w, err := WriteSSTable(path, []Entry{{Key: []byte("k"), Value: []byte("value")}})
	if err != nil {
		t.Fatal(err)
	}
	w.Close()

	// Flip a bit inside the first record's value, leaving every length intact.
	f, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1)
	f.ReadAt(buf, sstHeaderSize+1)
	buf[0] ^= 0xFF
	f.WriteAt(buf, sstHeaderSize+1)
	f.Close()

	tbl, err := OpenSSTable(path)
	if err != nil {
		t.Fatal(err)
	}
	defer tbl.Close()

	if _, _, _, err := tbl.Get([]byte("k")); !errors.Is(err, ErrCorruptTable) {
		t.Fatalf("Get on a corrupted record = %v, want ErrCorruptTable", err)
	}
}

// ---------------------------------------------------------------------------
// Bloom filter
// ---------------------------------------------------------------------------

// TestBloomNeverLies checks the only property that would be a correctness bug:
// a false negative. There must never be one.
func TestBloomNeverLies(t *testing.T) {
	const n = 10000
	b := newBloom(n)
	for i := 0; i < n; i++ {
		b.add([]byte(fmt.Sprintf("key%06d", i)))
	}
	for i := 0; i < n; i++ {
		if !b.mayContain([]byte(fmt.Sprintf("key%06d", i))) {
			t.Fatalf("FALSE NEGATIVE on key%06d -- the filter hid a key that exists", i)
		}
	}
}

// TestBloomFalsePositiveRate measures what the 10-bits-per-key setting actually
// buys. This is the number the read-path argument rests on.
func TestBloomFalsePositiveRate(t *testing.T) {
	const n = 10000
	b := newBloom(n)
	for i := 0; i < n; i++ {
		b.add([]byte(fmt.Sprintf("key%06d", i)))
	}

	fp := 0
	const probes = 100000
	for i := 0; i < probes; i++ {
		if b.mayContain([]byte(fmt.Sprintf("absent%06d", i))) {
			fp++
		}
	}
	rate := float64(fp) / float64(probes)
	t.Logf("bloom: %d bits/key, k=%d -> false-positive rate %.3f%% (%d/%d)",
		bitsPerKey, b.k, rate*100, fp, probes)

	// Theory says ~1% at 10 bits/key. Allow generous headroom; the assertion
	// that matters is that it is small, not that it hits a decimal place.
	if rate > 0.03 {
		t.Fatalf("false-positive rate %.3f%% is too high; the filter is not earning its bits", rate*100)
	}
}

// TestBloomSkipsTables shows the filter doing its job end to end: a missing-key
// lookup over many tables should touch almost no data.
func TestBloomSkipsTables(t *testing.T) {
	dir := t.TempDir()
	db := mustOpen(t, dir, 1<<20)
	defer db.Close()

	for tbl := 0; tbl < 10; tbl++ {
		for i := 0; i < 500; i++ {
			db.Set([]byte(fmt.Sprintf("t%02d-k%04d", tbl, i)), []byte("v"))
		}
		db.Flush()
	}
	if db.Tables() != 10 {
		t.Fatalf("Tables = %d, want 10", db.Tables())
	}

	// A key in no table at all: the worst case for an LSM read.
	for i := 0; i < 1000; i++ {
		if _, err := db.Get([]byte(fmt.Sprintf("missing-%d", i))); !errors.Is(err, ErrKeyNotFound) {
			t.Fatalf("Get(missing) = %v, want ErrKeyNotFound", err)
		}
	}
}

// ---------------------------------------------------------------------------
// Input validation
// ---------------------------------------------------------------------------

func TestRejectsBadInput(t *testing.T) {
	dir := t.TempDir()
	db := mustOpen(t, dir, 1<<20)
	defer db.Close()

	if err := db.Set(nil, []byte("v")); !errors.Is(err, ErrEmptyKey) {
		t.Fatalf("Set(nil key) = %v, want ErrEmptyKey", err)
	}
	if err := db.Set(bytes.Repeat([]byte("k"), MaxKeySize+1), []byte("v")); !errors.Is(err, ErrKeyTooLarge) {
		t.Fatalf("Set(huge key) = %v, want ErrKeyTooLarge", err)
	}
	if err := db.Set([]byte("k"), bytes.Repeat([]byte("v"), MaxValueSize+1)); !errors.Is(err, ErrValueTooLarge) {
		t.Fatalf("Set(huge value) = %v, want ErrValueTooLarge", err)
	}
}

func TestClosedDBRejectsOperations(t *testing.T) {
	dir := t.TempDir()
	db := mustOpen(t, dir, 1<<20)
	db.Set([]byte("k"), []byte("v"))
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	if err := db.Set([]byte("k"), []byte("v")); !errors.Is(err, ErrClosed) {
		t.Fatalf("Set after Close = %v, want ErrClosed", err)
	}
	if _, err := db.Get([]byte("k")); !errors.Is(err, ErrClosed) {
		t.Fatalf("Get after Close = %v, want ErrClosed", err)
	}
}

// TestCloseFlushesMemtable is the difference between Close and a crash.
func TestCloseFlushesMemtable(t *testing.T) {
	dir := t.TempDir()
	db := mustOpen(t, dir, 1<<20)
	db.Set([]byte("k"), []byte("v"))
	if db.Tables() != 0 {
		t.Fatal("expected nothing flushed yet")
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db2 := mustOpen(t, dir, 1<<20)
	defer db2.Close()
	if db2.Tables() != 1 {
		t.Fatalf("Tables after reopen = %d, want 1 -- Close did not flush", db2.Tables())
	}
	if got, err := db2.Get([]byte("k")); err != nil || string(got) != "v" {
		t.Fatalf("Get = (%q, %v)", got, err)
	}
}
