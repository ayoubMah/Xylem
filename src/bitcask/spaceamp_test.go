package bitcask

import (
	"fmt"
	"path/filepath"
	"testing"
)

// TestSpaceAmplification measures, rather than asserts, the central cost of the
// append-only design: an overwrite does not replace the old value, it buries
// it. The log therefore grows in proportion to the number of WRITES, while the
// live data set stays constant -- and Merge is the only thing that closes the
// gap.
//
// Space amplification is defined here as
//
//	on-disk bytes / bytes of live data
//
// which for an overwrite factor of N should approach N before Merge and 1.0
// after it.
//
// This is a measurement harness, not a pass/fail test. It emits the table used
// in the Evaluation chapter. Run it with:
//
//	go test -run TestSpaceAmplification -v ./bitcask/...
func TestSpaceAmplification(t *testing.T) {
	const liveKeys = 1000

	keys := make([][]byte, liveKeys)
	for i := range keys {
		keys[i] = []byte(fmt.Sprintf(benchKeyFmt, i))
	}
	val := make([]byte, benchValSize)
	for i := range val {
		val[i] = byte('a' + i%26)
	}

	// Bytes one record actually occupies on disk.
	recordSize := int64(HeaderSize + len(keys[0]) + benchValSize)
	liveBytes := recordSize * liveKeys

	t.Logf("live set: %d keys x %d B/record = %d B", liveKeys, recordSize, liveBytes)
	t.Logf("%-10s %14s %14s %10s %10s", "overwrites", "before(B)", "after(B)", "amp", "reclaimed")

	for _, overwrites := range []int{1, 2, 5, 10, 50} {
		path := filepath.Join(t.TempDir(), "xylem.log")
		s, err := Open(path, &Options{})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}

		for round := 0; round < overwrites; round++ {
			for _, k := range keys {
				if err := s.Set(k, val); err != nil {
					t.Fatalf("Set: %v", err)
				}
			}
		}

		before := s.Size()
		if err := s.Merge(); err != nil {
			t.Fatalf("Merge: %v", err)
		}
		after := s.Size()

		if got := s.Len(); got != liveKeys {
			t.Fatalf("after Merge: %d live keys, want %d", got, liveKeys)
		}

		ampBefore := float64(before) / float64(liveBytes)
		reclaimed := 100 * float64(before-after) / float64(before)
		t.Logf("%-10d %14d %14d %10.2f %9.1f%%", overwrites, before, after, ampBefore, reclaimed)

		if after != liveBytes {
			t.Errorf("overwrites=%d: after Merge log is %d B, want exactly %d B",
				overwrites, after, liveBytes)
		}
		if err := s.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}
}

// TestDeleteSpaceCost records the other half of the story: a delete makes the
// log BIGGER, because a tombstone is itself a record. The space is only
// returned at the next Merge, which drops both the tombstone and the record it
// buried.
func TestDeleteSpaceCost(t *testing.T) {
	const liveKeys = 1000

	path := filepath.Join(t.TempDir(), "xylem.log")
	s, err := Open(path, &Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	keys := make([][]byte, liveKeys)
	for i := range keys {
		keys[i] = []byte(fmt.Sprintf(benchKeyFmt, i))
	}
	val := make([]byte, benchValSize)

	for _, k := range keys {
		if err := s.Set(k, val); err != nil {
			t.Fatalf("Set: %v", err)
		}
	}
	afterWrites := s.Size()

	// Delete half of them.
	for _, k := range keys[:liveKeys/2] {
		if err := s.Delete(k); err != nil {
			t.Fatalf("Delete: %v", err)
		}
	}
	afterDeletes := s.Size()

	if err := s.Merge(); err != nil {
		t.Fatalf("Merge: %v", err)
	}
	afterMerge := s.Size()

	t.Logf("after %d writes:          %d B", liveKeys, afterWrites)
	t.Logf("after %d deletes:          %d B  (+%d B -- deletes GROW the log)",
		liveKeys/2, afterDeletes, afterDeletes-afterWrites)
	t.Logf("after Merge:               %d B  (%.1f%% of peak)",
		afterMerge, 100*float64(afterMerge)/float64(afterDeletes))

	if afterDeletes <= afterWrites {
		t.Errorf("deletes did not grow the log: %d -> %d", afterWrites, afterDeletes)
	}
	if got := s.Len(); got != liveKeys/2 {
		t.Errorf("%d live keys after deletes, want %d", got, liveKeys/2)
	}
}
