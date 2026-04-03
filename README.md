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

## Project Roadmap (Progress Log)

### Lab A: The B-Tree Decomposition
- [ ] **A.1: The Page & Serialization (The Physical Layer)**
  - *Goal:* Define the binary layout of a single disk block.
  - *Focus:* Fixed-size headers, Slotted Page architecture, and converting Go structs to `[]byte`.
- [ ] **A.2: The Disk Manager (The I/O Layer)**
  - *Goal:* Efficiently read/write pages to a file using pwrite and pread.
  - *Focus:* Mapping Page IDs to file offsets.
- [ ] **A.3: The Buffer Pool Manager (The Caching Layer)**
  - *Goal:* Manage a fixed-size cache of Pages in RAM.
  - *Focus:* The Replacement Policy (LRU/Clock), "Pinning" pages to prevent eviction while in use, and "Dirty" flags.
- [ ] **A.4: The Node Interface (The Structural Layer)**
  - *Goal:* Differentiate between Internal Nodes (pointers to other pages) and Leaf Nodes (actual Key-Value data).
  - *Focus:* Search within a single page (Binary Search).
- [ ] **A.5: The Tree Traversal (The Read Path)**
  - *Goal:* Implement `Get(key)`.
  - *Focus:* Navigating from Root to Leaf.
- [ ] **A.6: The Split (The Write Path)**
  - *Goal:* Handle page overflows during `Put(key, value)`.
  - *Focus:* Allocating new pages, re-balancing, and updating the parent.
- [ ] **A.7: The Delete (The Rebalancing Layer)**
  - *Goal:* Handle page underflows (merging or borrowing).
  - *Focus:* Managing the 50% occupancy rule.

### Phase 3: The LSM Engine
- [ ] MemTable: In-memory sorted buffers.
- [ ] SSTables: Immutable disk segments.
- [ ] Compaction: The "Garbage Collection" of storage engines.

### Phase 4: The Showdown (Benchmarking)
- [ ] Comparative analysis: Sequential vs. Random I/O.
- [ ] Write amplification study.

## Repository Structure
```text
.
├── internal/
│   ├── btree/       # Lab A: B-Tree implementation
│   ├── lsm/         # Lab B: Log-Structured Merge Tree
│   ├── buffer/      # Buffer Pool Management logic
│   └── wal/         # Write-Ahead Logging & Recovery
├── bench/           # Performance testing scripts
└── main.go          # CLI entry point for the lab
```

## Lab Notes & Learnings
> “A B-Tree is a balancing act of pointers; an LSM-Tree is a race against compaction.”

**Learning 01:** Implementing the Buffer Pool Manager taught me more about OS memory than three years of theory.

**Learning 02:** *(Add your "Aha!" moments here as you code)*

## How to Run the Lab
*(Coming soon as Phase 1 completes)*

```bash
go run main.go --engine=btree --bench
```
