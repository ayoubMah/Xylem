package lsm

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// ---------------------------------------------------------------------------
// THE DB -- where the read amplification bill arrives
// ---------------------------------------------------------------------------
//
// The memtable made writes cheap and the SSTable made flushes sequential. This
// file pays for both.
//
// A key now lives in a STACK of places, newest to oldest:
//
//	           newest
//	  +---------------------+
//	  |   memtable (RAM)    |  <- every write lands here
//	  +---------------------+
//	  |   SSTable  N        |  <- most recent flush
//	  |   SSTable  N-1      |
//	  |      ...            |
//	  |   SSTable  0        |  <- oldest flush
//	  +---------------------+
//	           oldest
//
// and the rules that make this correct are short but unforgiving:
//
//  1. SEARCH NEWEST FIRST, and STOP at the first table that has an opinion.
//     Not the first table with a VALUE -- the first with an opinion. A
//     tombstone is an opinion. Continuing past one resurrects a deleted key,
//     which is the classic LSM bug.
//  2. Older tables are never consulted once a newer one has answered, so
//     overwrites need no cleanup at write time. The old copy just becomes
//     unreachable, and compaction reclaims it later.
//
// The cost: a lookup that misses touches EVERY table. That is READ
// AMPLIFICATION, the R in the RUM conjecture, and it is what the bloom filters
// and compaction in this package exist to hold down.

// Options configures Open.
type Options struct {
	// MemTableSize is the flush threshold in bytes of key+value data. When the
	// memtable exceeds it, it is written out as an SSTable and a fresh one
	// takes over.
	//
	// This single number is the LSM's central dial. Larger means fewer, bigger
	// SSTables: fewer files to search on a read, more absorbed overwrites, more
	// RAM held, and more data at risk between flushes. Smaller means the
	// reverse. LevelDB's default is 4 MiB.
	MemTableSize int

	// SyncOnWrite fsyncs the WAL after every mutation.
	//
	// Same dial Phase 2 measured at 96.4x, and the same meaning: OFF survives a
	// process crash (the page cache outlives the process), ON survives power
	// loss. The difference in an LSM is WHERE the cost lands -- on the WAL
	// append, never on the SSTables, which are fsynced once per flush.
	SyncOnWrite bool
}

// DefaultMemTableSize matches LevelDB.
const DefaultMemTableSize = 4 << 20

// DB is a write-optimized key/value store.
//
// INVARIANTS:
//
//  1. tables is ordered NEWEST FIRST. Index 0 is the most recent flush.
//  2. Every key in the memtable is newer than every copy of it on disk.
//  3. seq is strictly increasing and never reused, so a file name encodes its
//     own age and the ordering survives a restart.
type DB struct {
	mu sync.RWMutex

	dir  string
	opts Options

	mem    *MemTable
	tables []*SSTable // newest first
	wal    *WAL

	seq    int
	closed bool
}

// Open opens or creates a database in dir.
//
// Recovery is the whole of startup and has two halves, in this order:
//
//  1. Load every SSTable found on disk, newest first. These are durable and
//     complete by construction -- an SSTable only exists if its footer was
//     written and fsynced.
//  2. Replay the WAL into a fresh memtable. That recovers every write accepted
//     since the last flush, which is exactly the data the SSTables do not have.
func Open(dir string, opts *Options) (*DB, error) {
	if opts == nil {
		opts = &Options{}
	}
	if opts.MemTableSize <= 0 {
		opts.MemTableSize = DefaultMemTableSize
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}

	db := &DB{
		dir:  dir,
		opts: *opts,
		mem:  NewMemTable(1),
	}

	// --- 1. load the SSTables ----------------------------------------------
	seqs, err := listTableSeqs(dir)
	if err != nil {
		return nil, err
	}
	// Descending: newest first, matching invariant (1).
	sort.Sort(sort.Reverse(sort.IntSlice(seqs)))
	for _, s := range seqs {
		t, err := OpenSSTable(tablePath(dir, s))
		if err != nil {
			db.Close()
			return nil, fmt.Errorf("lsm: opening table %06d: %w", s, err)
		}
		db.tables = append(db.tables, t)
		if s >= db.seq {
			db.seq = s + 1
		}
	}

	// --- 2. replay the WAL --------------------------------------------------
	wal, replayed, err := OpenWAL(filepath.Join(dir, "wal.log"), opts.SyncOnWrite)
	if err != nil {
		db.Close()
		return nil, err
	}
	db.wal = wal
	for _, e := range replayed {
		// Straight into the memtable, in file order, so later records win --
		// the same "replay in order and let the newest overwrite" recovery the
		// bitcask keydir uses.
		db.mem.put(e.Key, e.Value, e.Tombstone)
	}

	return db, nil
}

// Set stores value under key.
func (db *DB) Set(key, value []byte) error {
	return db.mutate(Entry{Key: key, Value: value})
}

// Delete writes a tombstone for key.
//
// It always succeeds, whether or not the key exists, and it always costs a
// write. See MemTable.Delete for why a delete cannot be an erase.
func (db *DB) Delete(key []byte) error {
	return db.mutate(Entry{Key: key, Tombstone: true})
}

func (db *DB) mutate(e Entry) error {
	if len(e.Key) == 0 {
		return ErrEmptyKey
	}
	if len(e.Key) > MaxKeySize {
		return ErrKeyTooLarge
	}
	if !e.Tombstone && len(e.Value) > MaxValueSize {
		return ErrValueTooLarge
	}

	db.mu.Lock()
	defer db.mu.Unlock()
	if db.closed {
		return ErrClosed
	}

	// WAL FIRST, memtable second. Never the other way round.
	//
	// This is write-ahead logging in one line, and the ordering is the entire
	// guarantee: if the log record is on the OS's side of the write() call
	// before the write is acknowledged, then any write the caller believes
	// succeeded can be replayed. Updating memory first would create a window
	// where a write is visible to readers but would vanish on restart.
	if err := db.wal.Append(e); err != nil {
		return err
	}
	db.mem.put(e.Key, e.Value, e.Tombstone)

	// Flush trigger. Checked on the write path because that is the only thing
	// that can make the memtable grow.
	if db.mem.Size() >= db.opts.MemTableSize {
		return db.flushLocked()
	}
	return nil
}

// Get returns the value stored under key.
//
// It returns ErrKeyNotFound when the key is absent OR deleted -- from the
// caller's side those are the same answer. Internally they are emphatically
// not, which is what the tombstone checks below are for.
func (db *DB) Get(key []byte) ([]byte, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	if db.closed {
		return nil, ErrClosed
	}

	// --- 1. the memtable: always newest ------------------------------------
	if v, found, tomb := db.mem.Get(key); found {
		if tomb {
			return nil, ErrKeyNotFound
		}
		return append([]byte(nil), v...), nil
	}

	// --- 2. the SSTables, newest to oldest ---------------------------------
	for _, t := range db.tables {
		v, found, tomb, err := t.Get(key)
		if err != nil {
			return nil, err
		}
		if !found {
			continue // no opinion here; ask the next-older table
		}
		if tomb {
			// STOP. This table says the key was deleted, and it is newer than
			// everything below it. Reading on would find the pre-delete value
			// and hand back a key the caller already removed.
			return nil, ErrKeyNotFound
		}
		return v, nil
	}

	return nil, ErrKeyNotFound
}

// Flush writes the current memtable out as a new SSTable, even if it has not
// reached the threshold. Exported so tests and benchmarks can force the
// boundary rather than wait for it.
func (db *DB) Flush() error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.closed {
		return ErrClosed
	}
	return db.flushLocked()
}

// flushLocked requires db.mu held for writing.
func (db *DB) flushLocked() error {
	if db.mem.Len() == 0 {
		return nil
	}

	entries := db.mem.Scan() // already sorted; see MemTable.Scan
	path := tablePath(db.dir, db.seq)

	t, err := WriteSSTable(path, entries)
	if err != nil {
		return err
	}

	// Prepend: this table is now the newest on disk. Invariant (1).
	db.tables = append([]*SSTable{t}, db.tables...)
	db.seq++

	// The SSTable is fsynced and durable, so every write it contains is now
	// recoverable without the log. Only NOW is it safe to reset the WAL.
	//
	// Ordering again: table durable -> log truncated. Truncating first would
	// open a window where a crash loses everything the memtable held.
	if err := db.wal.Reset(); err != nil {
		return err
	}

	db.mem = NewMemTable(1)
	return nil
}

// Tables reports how many SSTables are currently on disk. This is the number a
// missing-key lookup has to search, so it is the read-amplification factor and
// the thing compaction exists to reduce.
func (db *DB) Tables() int {
	db.mu.RLock()
	defer db.mu.RUnlock()
	return len(db.tables)
}

// Close flushes the memtable and releases every file handle.
func (db *DB) Close() error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.closed {
		return nil
	}

	var firstErr error
	if db.wal != nil && db.mem != nil && db.mem.Len() > 0 {
		if err := db.flushLocked(); err != nil {
			firstErr = err
		}
	}
	if db.wal != nil {
		if err := db.wal.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	for _, t := range db.tables {
		if err := t.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	db.closed = true
	return firstErr
}

// ---------------------------------------------------------------------------
// file naming
// ---------------------------------------------------------------------------

func tablePath(dir string, seq int) string {
	// Zero-padded so lexical order matches numeric order, which makes the
	// directory listing readable and any external tooling behave.
	return filepath.Join(dir, fmt.Sprintf("%06d.sst", seq))
}

func listTableSeqs(dir string) ([]int, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []int
	for _, e := range ents {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".sst") {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSuffix(name, ".sst"))
		if err != nil {
			continue // not one of ours; ignore rather than fail
		}
		out = append(out, n)
	}
	return out, nil
}
