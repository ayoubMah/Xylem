<p align="center">
  <img src="logo.png" width="300" alt="Xylem Logo"><br/>
  <b>Xylem: The Storage Lab</b>
</p>

**Mission:** Building a high-performance storage engine from the ground up to understand the "magic" behind persistence, memory management, and the B-Tree vs. LSM-Tree duality.

## The Experiment
Most databases choose a side. Xylem implements both. This project is a living laboratory based on the architectures explored in *Database Internals* by Alex Petrov.

The goal isn't just to store bytes—it's to measure exactly how different data structures behave under pressure.

## Tech Stack & Core Labs
**Language:** Go (Golang)

**Primary Text:** Database Internals (Alex Petrov)

**The Engines:**
- **Lab A (B-Tree):** Classic, read-optimized, pointer-based storage.
- **Lab B (LSM-Tree):** Modern, write-optimized, log-structured merge storage.

# Xylem Development Roadmap

## The Build Progression
**Rule:** Start small, ship each phase before starting the next. Each phase is a complete, working artifact — not a prototype. Phases are checkpoints. Sessions are preparation. You do NOT move to the next phase until the current one is shipped.

| Phase | What You Build | What It Proves |
| :--- | :--- | :--- |
| **Phase 1 — In-Memory KV** | Hash map + RWMutex, benchmarked | You understand hashing and concurrency at the data structure level. |
| **Phase 2 — Bitcask Clone** | Append-only log on disk, crash recovery | You understand file I/O, binary formats, and durability. |
| **Phase 3 — B-Tree on Disk** | Fixed pages, buffer pool, concurrent reads | You understand tree structure, disk layout, and page management. |
| **Phase 4 — LSM-Tree** | Memtable, SSTables, compaction | You understand write-optimized structures and read/write trade-offs. |
| **Phase 5 — Transactions** | MVCC, isolation levels | You understand the concurrency model that sits on top of storage. |

---

## Phase 1 — In-Memory KV Store
**Ship target:** A concurrent hash-table KV store in Go, benchmarked, race-clean.
**Central question:** A map is not magic. What is inside it, and what happens when two goroutines touch it at the same time?

*Before you persist data to disk, you need to understand what you're persisting. The simplest storage is just memory: a hash map with concurrent access control. Get this right first.*

### Session 1 — Hash Tables: What a Map Actually Is
**Read before this session:**
* "Database Internals" Ch 1 (Introduction and Overview)
* Go spec: Map types section
* Skim `src/runtime/map.go` in the Go source — read the `hmap` struct definition and `mapassign` comment.

> **The core question to sit with:**
> `m := make(map[string][]byte)` — what is `m`? Not "a map." Draw the memory layout. What is the struct? What happens when you insert the 8th key? The 1000th? Does `m` grow? What does growing cost?

**What to build:**
1. Write a Go program that implements a `HashMap` struct backed by `map[string][]byte` with `Set`, `Get`, `Delete` methods.
2. Measure how long `Set` takes for the first 100 entries vs entries 10,000–10,100 — is there a spike? Why?
3. Pre-allocate with `make(map[string][]byte, 100000)` and measure again.
4. Force a hash collision: implement your own hash function (FNV-32) on a `[]string` slice and find two strings that hash to the same bucket (mod 8).
5. Write `BenchmarkGet` and `BenchmarkSet` with `-benchmem` to record baseline ops/sec and allocs/op.

**CS Fundamental — Hash Tables:**
A hash table maps keys to values by hashing the key to an integer and reducing it to a bucket index. Go uses chaining with bucket arrays of 8 entries each. When the load factor exceeds 6.5, Go doubles the bucket array and incrementally rehashes all entries lazily so individual inserts don't spike.

### Session 2 — Concurrency: Protecting the Map
**Read before this session:**
* Go blog: "Introducing the Go Race Detector"
* Go standard library: `sync.RWMutex` docs
* "Database Internals" Ch 6 (Buffer Management) — first half.

> **The core question to sit with:**
> You have 10 goroutines reading and 2 goroutines writing. What breaks if you use no lock? What breaks if you use a single `sync.Mutex`? What does `sync.RWMutex` actually give you — and what does it cost?

**What to build:**
1. Use a plain `map[string][]byte` with 10 reader goroutines and 1 writer goroutine, no synchronization — run with `go test -race`.
2. Fix it with `sync.Mutex` — benchmark concurrent read throughput.
3. Fix it with `sync.RWMutex` — benchmark concurrent read throughput, compare to step 2.
4. Try `sync.Map` — benchmark the same read-heavy workload.

**DB Ship Checkpoint:**
* `Set(key string, value []byte)`
* `Get(key string) ([]byte, bool)`
* `Delete(key string)`
* Backed by `map[string][]byte` + `sync.RWMutex`
* `go test -race ./...` passes.

---

## Phase 2 — Persistence: Bitcask Clone
**Ship target:** A crash-safe KV store — data on disk, kill mid-write, restart, data consistent.
**Central question:** Your in-memory store dies with the process. How do you make writes survive a crash? How do you make reads fast when the data is on disk?

### Session 3 — File I/O at the Metal
**Read before this session:**
* "Database Internals" Ch 2 (B-Tree Basics) — "Disk-Based Structures"
* Linux man pages: `write(2)`, `fsync(2)`, `fdatasync(2)`, `open(2)`
* Go stdlib: `os.File.Sync()` doc comment

> **The core question to sit with:**
> You call `file.Write(data)`. Is the data on disk? If the power dies 1 second later, is it safe? What does "on disk" actually mean?

**What to build:**
1. Write 1MB to a file with `os.WriteFile` — measure time.
2. Open the file with `os.OpenFile` and call `file.Sync()` after every write — measure time.
3. Use `mmap` via `syscall.Mmap` to map a file into memory — read 1000 random 4KB pages.
4. Write 4KB records in a loop. After 500 records, kill the program with `os.Exit(1)`. On restart, count surviving records.

### Session 4 — Binary Format: Encoding Records on Disk
**Read before this session:**
* Bitcask paper — "Bitcask: A Log-Structured Hash Table for Fast Key/Value Data"
* "Database Internals" Ch 3 (File Formats)
* Go stdlib: `encoding/binary` package docs

> **The core question to sit with:**
> You want to write a key-value record to disk. What bytes do you write? In what order? How does the reader know where one record ends?

**What to build:**
1. Define a record format: `[CRC 4B][Timestamp 8B][KeyLen 4B][ValLen 4B][Key bytes][Value bytes]`.
2. Implement `WriteRecord(f *os.File, key, value []byte) (int64, error)`.
3. Implement `ReadRecord(f *os.File, offset int64) (key, value []byte, err error)`.
4. Write 1000 records, truncate mid-record, write a scan function that stops at the corrupt one cleanly.

### Session 5 — Crash Recovery: WAL and the In-Memory Index
**Read before this session:**
* Bitcask paper — Section 3 & 4
* "Database Internals" Ch 8 — WAL section
* BoltDB source: `db.go` — Open function recovery

> **The core question to sit with:**
> Your process starts. The log file has 1000 records. You need to rebuild the in-memory index. What if the log has 10 million records? 

**What to build (Full Bitcask Clone):**
1. In-memory index: `map[string]int64` (key → file offset).
2. `Open(path string)` — replays log, validates CRC, stops at corrupt record.
3. `Set`, `Get`, `Delete` methods.
4. `Merge()` — compactions to rewrite the log with only the latest value per key.

**DB Ship Checkpoint:**
* `Open`, `Set`, `Get`, `Delete`, `Merge` fully implemented.
* Kill mid-write, restart, all committed keys retrievable.

---

## Phase 3 — B-Tree on Disk
**Ship target:** A B-Tree KV store with fixed 4KB pages, buffer pool, and concurrent reads.
**Central question:** Why not just use the append-only log forever? What does a tree structure buy you that the log doesn't?

### Session 6 — B-Tree Theory: Why Trees and How They Split
**Read before this session:**
* "Database Internals" Ch 2 & 4
* BoltDB source: `bucket.go`

**What to build:**
1. Implement an in-memory B-Tree (order t=2).
2. Implement `Insert(key int)` with proper node splitting.
3. Implement `Search(key int) bool`.
4. Insert 1000 sequential and random keys, compare tree heights.

### Session 7 — Pages and the Buffer Pool
**Read before this session:**
* "Database Internals" Ch 3 & 6
* BoltDB source: `db.go`

**What to build:**
1. Define `Page [4096]byte` and `PageID uint64`.
2. Implement `DiskManager` with `WritePage` and `ReadPage`.
3. Implement a `BufferPool` (capacity 16 pages) mapping `PageID → *Page` with LRU eviction.
4. Serialize a B-Tree node into a Page: `[type 1B][num_keys 2B][keys ...][child_ids ...]`.

### Session 8 — B-Tree Concurrency: Readers and Writers
**Read before this session:**
* "Database Internals" Ch 7 (Latch coupling and MVCC)
* BoltDB source: `tx.go`

**What to build:**
1. Add `sync.RWMutex` at the tree level.
2. Implement a snapshot read `BeginRead() *Snapshot` returning a consistent point-in-time view.
3. Benchmark concurrent reads.

**DB Ship Checkpoint:**
* Fixed 4KB pages serialized to disk.
* Buffer pool with LRU eviction.
* `BenchmarkBTreeLookup` shows 0 allocs/op for reads.

---

## Phase 4 — LSM-Tree
**Ship target:** A write-optimized KV store with memtable, SSTables, and basic compaction.
**Central question:** What if you need to handle writes 10x faster than a B-Tree allows?

### Session 9 — LSM Design: Memtable and SSTables
**Read before this session:**
* "Database Internals" Ch 7
* LevelDB design doc: `doc/impl.md`
* LSM-Tree paper (O'Neil et al.) — Sections 1 & 2

**What to build:**
1. Implement `MemTable` as a sorted in-memory structure.
2. Implement `SSTable` — write memtable to disk in sorted order with a trailing index.
3. Implement flush trigger (e.g., at 4MB).
4. Implement multi-SSTable `Get()`.

### Session 10 — Compaction and the Read Path
**Read before this session:**
* "Database Internals" Ch 7 & 8 (Bloom filter section)
* LevelDB design doc (compaction)

**What to build:**
1. Implement `Compact()` using k-way merge.
2. Implement a Bloom filter for each SSTable to skip definite absences.
3. Implement size-tiered compaction triggers.

### Session 11 — WAL and Crash Recovery for LSM
**Read before this session:**
* "Database Internals" Ch 8
* LevelDB source: `db/log_writer.cc` and `log_reader.cc`

**What to build:**
1. Add WAL for all memtable operations using the binary format from Phase 2.
2. Implement WAL recovery on `Open()`.
3. Truncate/rotate WAL on memtable flush.

**DB Ship Checkpoint:**
* `Set`, `Get`, `Delete`, `Compact` methods fully functional.
* Bloom filters actively reducing disk seeks.
* LSM shows faster write ops/sec compared to B-Tree.

---

## Phase 5 — Transactions (Future Path)
*Begins after Phase 4 is shipped.*

**What it covers:**
* Isolation levels: read committed, repeatable read, serializable.
* MVCC: Consistent snapshots (PostgreSQL, InnoDB).
* 2PL (two-phase locking).
* Serializable Snapshot Isolation (SSI).

**Build target:** Add transactions to your B-Tree or LSM-Tree — begin, commit, rollback — with at least "read committed" isolation.

---

## CS Connections Map

| DB Concept | CS Fundamental |
| :--- | :--- |
| **Hash table load factor & rehashing** | Open addressing vs chaining, amortized complexity |
| **`sync.RWMutex` for KV store** | Reader-writer locking, starvation, locking granularity |
| **`fsync` and storage hierarchy** | Memory hierarchy: RAM → OS page cache → disk |
| **Binary record format with CRC** | Self-describing formats, error detection, crash-safe writes |
| **Append-only log with compaction** | Write-ahead logging, crash recovery, sequential vs random I/O |
| **B-Tree node splitting** | Balanced tree invariants, height bounds, branching factor |
| **Buffer pool with LRU eviction** | Cache replacement policies, dirty page management |
| **B-Tree concurrency** | Latch coupling, MVCC, copy-on-write |
| **LSM memtable → SSTable flush** | Write-optimized structures, write amplification |
| **Compaction k-way merge** | External sort, merge sort over sorted runs |
| **Bloom filter per SSTable** | Probabilistic data structures, false positive rate, bit arrays |
| **WAL for LSM recovery** | Write-ahead logging, at-least-once recovery, atomic rename |
