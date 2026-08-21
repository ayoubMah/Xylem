// Package lsm implements a write-optimized key/value store in the
// Log-Structured Merge-tree family (O'Neil et al., 1996; LevelDB; RocksDB).
//
// The whole design answers one question that Phase 2 and Phase 3 answered
// differently:
//
//	WHERE DOES A WRITE GO?
//
//	Bitcask (Phase 2): append one record to one log. O(1) sequential write,
//	                   but every key must live in RAM forever.
//	B-Tree  (Phase 3): find the leaf, then modify it IN PLACE. That is a
//	                   random write, and it is the expensive one.
//	LSM     (Phase 4): write into a SORTED IN-MEMORY table. Never touch the
//	                   disk on the write path at all. When that table gets
//	                   big, dump it to disk in ONE sequential pass, sorted,
//	                   and never modify it again.
//
// So an LSM converts random writes into sequential ones by buffering and
// sorting them in memory first. That is the entire trick. Everything else in
// this package -- SSTables, compaction, bloom filters -- exists to pay the bill
// that trick runs up on the READ path: a key may now live in any one of many
// immutable files, so a read has to look in more than one place.
//
// This file implements the in-memory half: the memtable.
package lsm

import (
	"bytes"
	"math/rand"
	"sync"
)

// ---------------------------------------------------------------------------
// WHY A SKIPLIST
// ---------------------------------------------------------------------------
//
// The memtable has exactly three requirements, and they rule out most
// structures between them:
//
//  1. ORDERED. It gets flushed to disk as a sorted file, so it must be able to
//     produce its keys in sorted order without an O(n log n) sort at flush
//     time -- flushing is on the critical path.
//  2. FAST OVERWRITE. Set(k, v) twice must not grow the structure. This is
//     what rules out reusing the Phase 3 B-Tree, which has set semantics and
//     rejects duplicates.
//  3. CONCURRENT READS while a write is in flight, cheaply.
//
// A sorted slice satisfies (1) but insert is O(n) memmove. A B-Tree satisfies
// (1) and (2) but every insert can restructure interior nodes, which makes
// lock-free reads genuinely hard. A skiplist satisfies all three and is the
// reason LevelDB, RocksDB and Badger all use one: it is a linked list, so an
// insert only ever PUBLISHES new nodes by repointing forward pointers. Nothing
// that a reader is currently standing on ever moves.
//
// The structure is a stack of linked lists. The bottom list (level 0) holds
// every node in sorted order. Each list above it holds a random ~1/4 subset of
// the list below. Searching walks the top list until the next key would
// overshoot, drops a level, and repeats -- so each level skips roughly 4x the
// distance of the one below, and the search is O(log n) with base 1/p.
//
//	L3  HEAD ------------------------------------> [m] ------------> NIL
//	L2  HEAD ---------------> [f] ---------------> [m] ------------> NIL
//	L1  HEAD -------> [c] --> [f] -------> [k] --> [m] --> [q] ----> NIL
//	L0  HEAD -> [a] -> [c] -> [f] -> [h] -> [k] -> [m] -> [q] -> [z] NIL
//
// Looking for "k": start at L3, next is [m] which is > k, drop. L2, next is
// [f] < k so step to it, next is [m] > k, drop. L1, next is [k] -- found.
// Four comparisons instead of six.
const (
	// maxHeight caps the tower. 12 levels at p=1/4 addresses 4^12 = ~16.7M
	// entries before the top level stops being selective, which is far above
	// any memtable we will ever flush (default threshold is 4 MiB).
	maxHeight = 12

	// branch is the inverse of p. A node gets promoted to the next level up
	// with probability 1/branch. 4 is LevelDB's choice: it trades a little
	// search speed for materially less pointer memory than the textbook 2.
	branch = 4
)

// node is one entry in the skiplist.
//
// next is the node's tower: next[i] is the following node at level i. len(next)
// IS the node's height, decided once at insert time and never changed.
type node struct {
	key   []byte
	value []byte

	// tombstone marks a deletion.
	//
	// A delete in an LSM is a WRITE, not an erase -- the same insight as
	// Bitcask's tombstone record, for a different reason. Here the key may also
	// exist in older SSTables on disk, and those files are immutable. The only
	// way to say "this key is gone" is to write a newer entry that shadows
	// them. The tombstone cannot be dropped until compaction has merged away
	// every older entry it could possibly be hiding.
	tombstone bool

	next []*node
}

// MemTable is a sorted, in-memory, mutable table.
//
// INVARIANTS:
//
//  1. Keys at every level are in strictly ascending bytes.Compare order.
//  2. A key appears at most once. Set on an existing key overwrites in place,
//     so the structure never grows on overwrite.
//  3. size is the running total of live key+value bytes, so the flush trigger
//     never has to walk the list to find out how big it is.
type MemTable struct {
	// One RWMutex. Reads are concurrent; a write excludes. A production LSM
	// makes the skiplist genuinely lock-free on the read path with atomic
	// pointer loads -- the structure is chosen to allow exactly that -- but a
	// mutex is the honest simple version and the benchmark measures what we
	// actually built, not what we could have.
	mu sync.RWMutex

	head   *node
	height int // current number of levels in use, 1..maxHeight
	n      int // number of live entries

	// size is the approximate in-memory footprint used by the flush trigger:
	// key bytes + value bytes only. It deliberately ignores pointer overhead,
	// because the number it needs to predict is the size of the SSTABLE this
	// table will become, and pointers are not written to disk.
	size int

	rng *rand.Rand
}

// NewMemTable returns an empty memtable.
//
// seed makes the tower heights reproducible, which matters for tests: a
// skiplist's shape is randomised, and a test that asserts on height needs that
// randomness to be the same every run.
func NewMemTable(seed int64) *MemTable {
	return &MemTable{
		// The head is a sentinel: it holds no key and always has the maximum
		// height, so a search can always start at the top level without a
		// special case for an empty list.
		head:   &node{next: make([]*node, maxHeight)},
		height: 1,
		rng:    rand.New(rand.NewSource(seed)),
	}
}

// Len reports the number of distinct keys held, tombstones included.
func (m *MemTable) Len() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.n
}

// Size reports the approximate key+value bytes held. This is what the flush
// trigger compares against its threshold.
func (m *MemTable) Size() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.size
}

// Height reports how many levels are currently in use. Exposed for the
// experiments in the tests -- it is the observable consequence of the random
// promotion, and should sit near log_4(n).
func (m *MemTable) Height() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.height
}

// randomHeight draws a tower height: 1, plus one more level for each successful
// 1-in-branch coin flip, capped at maxHeight.
//
// This is the only randomness in the structure, and it is what replaces the
// explicit rebalancing a B-Tree has to do. A B-Tree guarantees balance by
// splitting nodes; a skiplist gets it in expectation, for free, by making the
// shape independent of the insertion order. Phase 3 measured what happens to a
// naive BST fed ascending keys: height 100,000. A skiplist fed the same
// ascending keys is unaffected, because the coin does not know the key.
func (m *MemTable) randomHeight() int {
	h := 1
	for h < maxHeight && m.rng.Intn(branch) == 0 {
		h++
	}
	return h
}

// findPrev locates, for every level, the last node whose key is < key.
//
// That vector is exactly what an insert needs (the nodes whose forward pointers
// must be repointed at the new node) and its level-0 entry is what a lookup
// needs (its successor is the candidate match). One traversal serves both,
// which is why this is the only search routine in the file.
//
// prev must be a caller-supplied array so that lookups do not allocate.
func (m *MemTable) findPrev(key []byte, prev *[maxHeight]*node) *node {
	x := m.head

	// Walk from the highest level in use down to level 0. At each level, step
	// forward while the NEXT key is still < key; when it would overshoot,
	// record where we stopped and drop a level.
	for i := m.height - 1; i >= 0; i-- {
		for x.next[i] != nil && bytes.Compare(x.next[i].key, key) < 0 {
			x = x.next[i]
		}
		prev[i] = x
	}

	// x is now the last node < key at level 0, so its successor is the only
	// node that can possibly equal key.
	return x.next[0]
}

// Set inserts or overwrites key with value.
//
// The value is COPIED. The caller owns their slice and may reuse it the
// instant this returns; storing it directly would leave the memtable holding a
// window into memory that changes underneath it. Same reasoning as
// DecodeRecord's copy-don't-sub-slice in the bitcask package -- the principle
// applied a second time.
func (m *MemTable) Set(key, value []byte) {
	m.put(key, value, false)
}

// Delete writes a tombstone for key.
//
// Note that this can make the memtable BIGGER. Deleting a key that is not
// present still inserts a node, because we cannot know from here whether an
// older SSTable on disk holds a value that needs shadowing. Phase 2 measured
// the same effect on the log: 500 tombstones cost +16,000 bytes.
func (m *MemTable) Delete(key []byte) {
	m.put(key, nil, true)
}

func (m *MemTable) put(key, value []byte, tombstone bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var prev [maxHeight]*node
	found := m.findPrev(key, &prev)

	// --- OVERWRITE PATH ----------------------------------------------------
	//
	// The key is already here. Replace the value in place: no new node, no
	// pointer surgery, and the entry count does not move. This is requirement
	// (2) at the top of the file, and it is why a write-heavy workload over a
	// small key space costs the memtable nothing to absorb.
	if found != nil && bytes.Equal(found.key, key) {
		m.size += len(value) - len(found.value)
		found.value = append([]byte(nil), value...)
		found.tombstone = tombstone
		return
	}

	// --- INSERT PATH -------------------------------------------------------
	h := m.randomHeight()

	// If this node is taller than anything built so far, the levels between the
	// old height and the new one have never been walked. Their predecessor is
	// the head sentinel by definition, so seed those entries before use.
	if h > m.height {
		for i := m.height; i < h; i++ {
			prev[i] = m.head
		}
		m.height = h
	}

	n := &node{
		key:       append([]byte(nil), key...),
		value:     append([]byte(nil), value...),
		tombstone: tombstone,
		next:      make([]*node, h),
	}

	// Splice in, bottom-up. Each level is an ordinary singly-linked-list
	// insertion: point the new node at what the predecessor pointed at, then
	// point the predecessor at the new node.
	//
	// Bottom-up matters for the lock-free version this shape is chosen to
	// allow: a reader that observes the node at level i always finds it fully
	// linked at every level below i, so it can never fall off the list.
	for i := 0; i < h; i++ {
		n.next[i] = prev[i].next[i]
		prev[i].next[i] = n
	}

	m.n++
	m.size += len(key) + len(value)
}

// Get returns the value for key.
//
// The three-way return is the whole reason a tombstone exists, and callers must
// not collapse it into two:
//
//	(v,   true,  false) -- key is live here, v is the answer
//	(nil, true,  true)  -- key is DELETED here; the answer is "not found" and
//	                       the caller must STOP. Older SSTables may still hold
//	                       a value, and returning it would resurrect the key.
//	(nil, false, false) -- key is not in this table at all; keep looking.
func (m *MemTable) Get(key []byte) (value []byte, found bool, tombstone bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var prev [maxHeight]*node
	x := m.findPrev(key, &prev)
	if x == nil || !bytes.Equal(x.key, key) {
		return nil, false, false
	}
	if x.tombstone {
		return nil, true, true
	}
	return x.value, true, false
}

// Entry is one memtable record, as handed to the SSTable writer.
type Entry struct {
	Key       []byte
	Value     []byte
	Tombstone bool
}

// Scan returns every entry in ascending key order.
//
// This is the flush path, and it is O(n) with no comparisons at all: level 0 is
// already a sorted linked list, so "sorting" the memtable is just walking it.
// That is requirement (1), and it is why the flush can be one sequential write
// with no buffering beyond the file itself.
func (m *MemTable) Scan() []Entry {
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make([]Entry, 0, m.n)
	for x := m.head.next[0]; x != nil; x = x.next[0] {
		out = append(out, Entry{
			Key:       x.key,
			Value:     x.value,
			Tombstone: x.tombstone,
		})
	}
	return out
}
