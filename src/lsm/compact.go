package lsm

import (
	"bytes"
	"container/heap"
	"os"
)

// ---------------------------------------------------------------------------
// COMPACTION
// ---------------------------------------------------------------------------
//
// Every design decision so far has deferred work rather than doing it:
//
//	an overwrite does not erase the old value  -> it shadows it
//	a delete does not erase anything           -> it writes a tombstone
//	a flush does not merge with what is there  -> it adds another file
//
// Compaction is where that deferred work is finally paid, and it is the only
// process in the engine that REMOVES data. It reads several SSTables, merges
// them into one that keeps only the newest version of each key, writes it, and
// deletes the inputs.
//
// It buys back both of the things the write path spent:
//
//   - READ AMPLIFICATION. A missing-key lookup searches every table. Merging
//     eight tables into one makes that lookup 8x cheaper.
//   - SPACE AMPLIFICATION. Phase 2 measured this on the bitcask log and found
//     space amplification equals the overwrite factor EXACTLY. The same holds
//     here, and compaction is this engine's Merge().
//
// What it costs is WRITE amplification: every byte it keeps is written to disk
// a second time. That is the third term of the RUM conjecture, and the reason
// there is no free lunch -- read, update and memory costs can be traded against
// each other, never all optimised at once. An LSM's whole personality is
// choosing cheap writes now and paying for them later, in the background.

// mergeSource is one table's worth of entries being consumed by the merge.
type mergeSource struct {
	entries []Entry
	pos     int

	// age orders sources by recency: 0 is the newest table. It is the
	// tiebreaker that makes the merge correct, and it is the only place the
	// "newest wins" rule is actually enforced.
	age int
}

func (s *mergeSource) cur() Entry { return s.entries[s.pos] }
func (s *mergeSource) done() bool { return s.pos >= len(s.entries) }
func (s *mergeSource) advance()   { s.pos++ }

// mergeHeap orders sources by (current key ascending, age ascending).
//
// The second term is load-bearing. When two tables both hold key k, the heap
// must surface the one from the NEWER table first, because the merge keeps the
// first occurrence of each key and discards the rest. Getting this comparison
// backwards would silently resurrect old values -- and it would pass any test
// that never writes the same key twice.
type mergeHeap []*mergeSource

func (h mergeHeap) Len() int { return len(h) }
func (h mergeHeap) Less(i, j int) bool {
	c := bytes.Compare(h[i].cur().Key, h[j].cur().Key)
	if c != 0 {
		return c < 0
	}
	return h[i].age < h[j].age
}
func (h mergeHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *mergeHeap) Push(x any)   { *h = append(*h, x.(*mergeSource)) }
func (h *mergeHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}

// mergeEntries performs a k-way merge over sources ordered newest-first.
//
// dropTombstones must be true ONLY when every table in the database is part of
// this merge. See Compact for why.
func mergeEntries(sources []*mergeSource, dropTombstones bool) []Entry {
	h := &mergeHeap{}
	for _, s := range sources {
		if !s.done() {
			*h = append(*h, s)
		}
	}
	heap.Init(h)

	out := make([]Entry, 0, 1024)
	var last []byte
	haveLast := false

	for h.Len() > 0 {
		s := (*h)[0]
		e := s.cur()

		// The heap guarantees this is the newest surviving copy of e.Key the
		// first time we see that key. Every later copy is older by definition,
		// so it is dropped without being examined.
		if !haveLast || !bytes.Equal(last, e.Key) {
			last = append(last[:0], e.Key...)
			haveLast = true

			switch {
			case !e.Tombstone:
				out = append(out, e)
			case !dropTombstones:
				// Keep the tombstone. There are older tables outside this merge
				// that may still hold a value for this key, and the tombstone
				// is the only thing shadowing them.
				out = append(out, e)
			default:
				// Full merge: nothing older exists anywhere, so there is
				// nothing left to shadow and the tombstone can finally go.
				// This is the only moment a delete actually frees space.
			}
		}

		s.advance()
		if s.done() {
			heap.Pop(h)
		} else {
			heap.Fix(h, 0)
		}
	}

	return out
}

// Compact merges every SSTable into a single new one.
//
// This is a FULL compaction, which is the simplest correct kind and the only
// kind that may drop tombstones. The rule is worth stating precisely because
// getting it wrong is a data-resurrection bug that hides for a long time:
//
//	A tombstone may be discarded only when the merge includes the OLDEST
//	table in the database. Otherwise some older table still holds a value
//	the tombstone was hiding, and dropping it un-deletes the key.
//
// A production engine compacts subsets (size-tiered or levelled) to avoid
// rewriting the whole dataset at once, and therefore has to track exactly which
// tombstones are safe to drop. That refinement is designed and not built here;
// what is built is the version whose correctness is obvious.
func (db *DB) Compact() error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.closed {
		return ErrClosed
	}
	if len(db.tables) < 2 {
		// Nothing to merge. One table is already the compacted form of itself,
		// and compacting it would rewrite every byte for no benefit.
		return nil
	}

	// db.tables is newest-first, so the slice index IS the age.
	sources := make([]*mergeSource, 0, len(db.tables))
	for age, t := range db.tables {
		entries, err := t.Scan()
		if err != nil {
			return err
		}
		sources = append(sources, &mergeSource{entries: entries, age: age})
	}

	merged := mergeEntries(sources, true /* full merge: tombstones may go */)

	// Write the result BEFORE touching the inputs. If the process dies here,
	// the old tables are all still on disk and intact, and the half-written new
	// one is removed by WriteSSTable's own cleanup. Recovery finds exactly the
	// pre-compaction state -- correct, just not yet compacted.
	newPath := tablePath(db.dir, db.seq)
	t, err := WriteSSTable(newPath, merged)
	if err != nil {
		return err
	}
	db.seq++

	// Now swap. The new table is durable, so the old ones are redundant.
	old := db.tables
	db.tables = []*SSTable{t}

	var firstErr error
	for _, ot := range old {
		if err := ot.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		if err := os.Remove(ot.Path()); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// MaybeCompact runs a full compaction once the table count reaches threshold.
//
// This is the simplest possible compaction POLICY -- the mechanism above is
// separate from it on purpose. Real engines choose between size-tiered (merge
// tables of similar size; cheaper writes, worse reads and space) and levelled
// (keep each level non-overlapping; better reads and space, more write
// amplification). That choice is the single biggest performance decision in an
// LSM, and it is a policy question, not a correctness one.
func (db *DB) MaybeCompact(threshold int) error {
	db.mu.RLock()
	n := len(db.tables)
	db.mu.RUnlock()

	if n >= threshold {
		return db.Compact()
	}
	return nil
}
