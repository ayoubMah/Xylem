<p align="center">
  <img src="logo.png" width="300" alt="Xylem Logo"><br/>
  <b>Xylem: The Storage Lab</b>
</p>

# Building a Database from Scratch — Go

A ground-up implementation of database internals in Go. Not using a database — building one, layer by layer, from memory to disk to crash recovery.

**Theme:** A database is a storage problem, a concurrency problem, and a crash-recovery problem. This project solves all three.

---

## What Gets Built

### Phase 1 — In-Memory KV Store
A concurrent hash-map KV store, benchmarked and race-clean.

- `HashMap` backed by `map[string][]byte` + `sync.RWMutex`
- Benchmarked with `-benchmem`: ops/sec, allocs/op
- Passes `go test -race ./...`

**Proves:** hashing, load factors, locking granularity

---

### Phase 2 — Bitcask Clone
A crash-safe KV store: append-only log on disk, binary record format, in-memory index, compaction.

- Binary record format: `[CRC 4B][Timestamp 8B][KeyLen 4B][ValLen 4B][Key][Value]`
- `fsync` after every write — durability, not convenience
- On restart: replay log, rebuild index, stop at first bad CRC
- `Merge()`: rewrites log with latest value per key, tombstones removed

**Proves:** file I/O, `fsync` vs OS page cache, crash recovery, write-ahead logging

---

### Phase 3 — B-Tree on Disk
A B-Tree with fixed 4KB pages, a buffer pool, and concurrent readers.

- Pages serialized as `[4096]byte` structs, addressed by `PageID`
- `DiskManager`: `ReadPage` / `WritePage` via `file.ReadAt` / `file.WriteAt`
- `BufferPool`: LRU eviction, dirty-page tracking, 0 allocs/op on cached reads
- Concurrent reads with latch coupling or tree-level `sync.RWMutex`

**Proves:** tree structure, disk layout, page management, buffer pool eviction

---

### Phase 4 — LSM-Tree
A write-optimized KV store: memtable → SSTables → compaction, with WAL for crash recovery.

- `MemTable`: sorted in-memory buffer, flushed to disk at 4MB
- `SSTable`: immutable sorted file + index + Bloom filter
- `Compact()`: k-way merge over SSTables, removes stale values and tombstones
- WAL: every write logged before touching the memtable; replayed on restart

**Proves:** write amplification vs read amplification, Bloom filters, LSM recovery semantics

---

### Phase 5 — Transactions *(future)*
MVCC, isolation levels, 2PL, Serializable Snapshot Isolation — on top of the B-Tree or LSM-Tree.

---

## Sources

| Source | Used for |
|--------|---------|
| *Database Internals* — Alex Petrov | Primary text: storage engines, B-Trees, LSM, concurrency |
| Bitcask paper | Log-structured hash table design |
| BoltDB source (`go.etcd.io/bbolt`) | B-Tree in Go, mmap, copy-on-write transactions |
| LevelDB design doc (`doc/impl.md`) | LSM design decisions from production |

---

## Ship Conditions

Each phase must ship before the next starts. "Ship" means:

| Phase | Ship condition |
|-------|---------------|
| 1 | `go test -race ./...` passes, `BenchmarkGet` and `BenchmarkSet` recorded |
| 2 | Kill mid-write, restart, all committed keys retrievable; compaction verified with file sizes |
| 3 | `BenchmarkBTreeLookup -benchmem` shows 0 allocs/op; race-clean |
| 4 | Write ops/sec > Phase 3 B-Tree; crash recovery tested; compaction verified |

