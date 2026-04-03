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

### Phase 1: The Foundation
- [ ] The Page: Implementing fixed-size disk blocks.
- [ ] Buffer Pool Manager: Moving pages between disk and RAM without blowing up memory.
- [ ] WAL (Write-Ahead Log): Ensuring "Atomic" and "Durable" aren't just buzzwords.

### Phase 2: The B-Tree Engine
- [ ] Node splitting & merging algorithms.
- [ ] Disk-based persistence (traversing files, not just RAM).

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
