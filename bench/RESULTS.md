# Xylem — Benchmark Results

**Recorded:** 2026-08-21
**Commit:** `7e4589a` (Phase 2 — Bitcask)
**Source:** `src/bitcask/bench_test.go`, `src/bitcask/rawio_test.go`, `src/bitcask/spaceamp_test.go`

Every number quoted in the thesis comes from this file. Nothing here is reconstructed from
memory or rounded for effect.

---

## 1. Experimental setup

| | |
|---|---|
| CPU | 13th Gen Intel Core i5-1335U — 12 logical cores, **15 W mobile part** |
| RAM | 15.7 GB |
| Disk | Samsung NVMe BM9C1, 512 GB (477 GiB) |
| OS | Windows 11 Pro 10.0.26200 |
| Go | go1.26.2 windows/amd64 |
| Target dir | `%TEMP%` on the NVMe system volume (via `b.TempDir()`) |

**Workload parameters**, fixed in code:

| | |
|---|---|
| Key | `"key:%08d"` → **12 B** |
| Value | **100 B** |
| Record on disk | 20 (header) + 12 (key) + 100 (value) = **132 B** |
| Read corpus | 10,000 keys |

---

## 2. Threats to validity — read this before quoting any absolute number

This is a **laptop**, not a controlled benchmark host, and the report must say so.

1. **Frequency scaling and thermal throttling.** The i5-1335U is a 15 W part. Across ten
   repetitions `BenchmarkGet` returned ~31 µs eight times and ~9 µs twice — a **bimodal**
   distribution, not noise around a mean. `BenchmarkSet` drifted the other way, from ~13 µs in the
   first five runs to ~23 µs in the last five, which is the signature of thermal throttling.
2. **Windows real-time antivirus** intercepts file I/O. A bare 132-byte `ReadAt` measured at a
   median of **13.75 µs**; the same call on Linux is typically ~1 µs. The absolute I/O floor here
   is roughly an order of magnitude above a server baseline.
3. **`-race` could not be run on this machine.** `go test -race` requires cgo and no C toolchain is
   installed. The concurrency tests pass, but the race detector has not been run against Phase 2.

**Consequence for the thesis.** Absolute latencies are **indicative of this machine only**. The
defensible results are the **ratios**, because both sides of every ratio were measured in the same
process, on the same file system, within seconds of each other. All ratios below are
median-over-median, `n = 10` (`n = 5` for recovery and merge).

> **Recommended upgrade:** re-run on one of the DRNA lab VMs (Linux, no AV interception, gcc
> present so `-race` works). That would tighten the absolute numbers *and* close the `-race` gap.
> The ratios are not expected to change qualitatively; the syscall floor should drop sharply.

---

## 3. Core results

Median, with min–max across repetitions. `n = 10` unless noted.

| Benchmark | Median | Range | Spread | B/op | allocs/op |
|---|---:|---:|---:|---:|---:|
| `Set` (buffered) | **20.84 µs** | 12.89 – 23.86 µs | 1.9× | 232 | 2 |
| `SetSync` (fsync per write) | **2.01 ms** | 1.84 – 2.48 ms | 1.3× | 250 | 2 |
| `SetOverwrite` (same key) | 21.95 µs | 21.09 – 23.42 µs | 1.1× | 136 | 2 |
| `Get` | **31.16 µs** | 8.66 – 32.55 µs | 3.8× | 360 | 5 |
| `GetParallel` | 13.14 µs | 12.84 – 13.95 µs | 1.1× | 360 | 5 |
| `MemSet` (Phase 1 baseline) | 629.5 ns | 296.6 – 845.0 ns | 2.8× | 16 | 1 |
| `MemGet` (Phase 1 baseline) | **192.6 ns** | 121.9 – 263.8 ns | 2.2× | 0 | 0 |

**Raw syscall floor** — same file system, no engine at all:

| Benchmark | Median | Range | B/op | allocs/op |
|---|---:|---:|---:|---:|
| `RawWriteAt` (1 × 132 B) | 20.83 µs | 13.67 – 22.23 µs | 0 | 0 |
| `RawReadAt` (1 × 132 B) | 13.75 µs | 7.61 – 20.23 µs | 0 | 0 |
| `RawReadAtTwice` (header + record) | 17.96 µs | 15.84 – 23.02 µs | 0 | 0 |
| `RawSync` (write + fsync) | 1.69 ms | 1.45 – 3.15 ms | 3 | 0 |

Derived throughput (median, single-threaded unless noted):

| Path | ops/s |
|---|---:|
| In-memory read (Phase 1) | ~5,190,000 |
| In-memory write (Phase 1) | ~1,589,000 |
| Bitcask read, 12 goroutines | ~76,100 |
| Bitcask write, buffered | ~48,000 |
| Bitcask read, single-threaded | ~32,100 |
| **Bitcask write, fsync per write** | **~500** |

---

## 4. The four findings

### F1 — fsync costs ~96×, and it does not buy what people think it buys

| | |
|---|---:|
| `SetSync` / `Set` (engine) | **96.4×** |
| `RawSync` / `RawWriteAt` (raw syscall) | **81.4×** |

The two agree, so the cost is the platform's flush, not anything Xylem does. Throughput collapses
from ~48,000 writes/s to **~500 writes/s**.

What the dial actually controls:

- **Off** — `write(2)` has returned; the data is in the OS page cache. A **process** crash
  (`panic`, `os.Exit`, `kill -9`) loses **nothing**: the kernel still owns the bytes.
- **On** — the data is on the physical device. Survives **power loss** and kernel panic.

So fsync is not what makes a process crash survivable. It is what makes *power loss* survivable,
and on this machine that costs two orders of magnitude. `TestCrashRecovery` passes with
`SyncOnWrite: false` — which is the empirical demonstration of exactly this distinction.

### F2 — the write path is free; the engine is a rounding error on it

| | |
|---|---:|
| `Set` / `RawWriteAt` | **1.0×** |

Encoding the record, computing CRC32 over it, taking the mutex and updating the keydir together
cost **nothing measurable** against a single positional write. The append-only design puts the
engine entirely in the shadow of one syscall.

### F3 — the read path is syscall-bound, and `ReadRecord` pays for two of them

| | |
|---|---:|
| `Get` / `RawReadAtTwice` | 1.7× |
| `RawReadAtTwice` / `RawReadAt` | 1.3× |

`ReadRecord` reads the fixed 20-byte header, learns the body length, then reads the whole record —
**two syscalls where one would do**. The source comment predicted this cost before it was measured
("a production engine would read a larger speculative chunk to save the second syscall"); the
benchmark prices it at ~1.3× on the raw floor. The remaining gap to `Get` is CRC verification and
**5 allocations per read** (360 B/op for a 132 B record).

This is the single clearest optimisation target in Phase 2: one speculative read of, say, 4 KiB
would serve the header and body together and eliminate the second syscall.

### F4 — persistence is asymmetric: it costs 33× on writes and 162× on reads

| | |
|---|---:|
| `Set` / `MemSet` | **33.1×** |
| `Get` / `MemGet` | **161.7×** |

The append-only log makes *writing* cheap to persist — one sequential append, no seek, no
in-place update. It makes *reading* expensive, because every read leaves user space even though
the keydir already knows exactly where to go. That asymmetry is the whole reason the next two
phases exist, and it is the empirical form of the read/write trade-off.

`GetParallel` at 13.14 µs against `Get` at 31.16 µs shows the `RWMutex` doing its job: readers
scale (**2.4× faster per operation** with 12 goroutines), because the lock is shared and the
positional reads do not contend.

---

## 5. Recovery — linear in the log, not in the live set

`Open()` **is** the recovery path: replay from byte 0, newest write wins by file order, stop at the
first torn or corrupt record. `n = 5`.

| Records | Log size | Median | Per record | B/op | allocs/op |
|---:|---:|---:|---:|---:|---:|
| 1,000 | 0.13 MiB | 32.10 ms | 32.10 µs | 485,911 | 6,028 |
| 10,000 | 1.26 MiB | 180.45 ms | 18.04 µs | 4,637,185 | 60,095 |
| 100,000 | 12.59 MiB | 1,810.31 ms | 18.10 µs | 44,616,624 | 600,603 |

**10× the records → 10.0× the time** between 10k and 100k. Recovery is exactly linear, and settles
at **~18 µs per record**. The 1k case is higher per-record (32 µs) because fixed open costs have
not yet amortised.

Two things this table says that matter more than the timings:

1. **Startup cost is a function of how many records were ever WRITTEN, not how much data is live.**
   A store holding 1,000 live keys that has been overwritten 100 times pays a 100,000-record
   recovery. This is the structural weakness of the Bitcask model, and it is why `Merge()` is not
   an optimisation but a requirement.
2. **6.0 allocations and ~486 B per record replayed**, for records that are 132 B on disk —
   **3.7× allocation amplification**, perfectly linear in record count. `ReadRecord` allocates a
   header buffer, a record buffer, the `Record` itself, and separate copies of key and value. A
   recovery path that reused one buffer would cut this substantially.

---

## 6. Space amplification

`TestSpaceAmplification` — 1,000 live keys × 132 B = **132,000 B of live data**, overwritten *N*
times, measured before and after `Merge()`.

| Overwrites | Before (B) | After (B) | Amplification | Reclaimed |
|---:|---:|---:|---:|---:|
| 1 | 132,000 | 132,000 | 1.00× | 0.0 % |
| 2 | 264,000 | 132,000 | 2.00× | 50.0 % |
| 5 | 660,000 | 132,000 | 5.00× | 80.0 % |
| 10 | 1,320,000 | 132,000 | 10.00× | 90.0 % |
| 50 | 6,600,000 | 132,000 | 50.00× | 98.0 % |

Space amplification equals the overwrite factor **exactly**, and `Merge()` returns the log to
precisely the live-data size every time. That is the append-only bargain stated numerically: the
log grows with the number of **writes**, never with the amount of **data**.

`BenchmarkMerge` — compacting 10,000 records down to 1,000 live keys: **119.11 ms** median
(102.27 – 148.14 ms, `n = 5`), ≈ 11.9 µs per record processed.

### Deletes make the log bigger

`TestDeleteSpaceCost` — 1,000 keys written, then 500 deleted:

| Stage | Size | Δ |
|---|---:|---:|
| after 1,000 writes | 132,000 B | — |
| after 500 deletes | 148,000 B | **+16,000 B** |
| after `Merge()` | 66,000 B | 44.6 % of peak |

A delete cannot erase bytes from an append-only log, so it **appends** a tombstone: 20 B header +
12 B key + 0 B value = **32 B**, and 500 × 32 = 16,000 B exactly. The space is only returned at the
next `Merge()`, which drops both the tombstone and the record it buried.

---

## 7. Reproduction

```sh
cd src

# Core + baseline benchmarks (as recorded above)
go test -bench='^BenchmarkSet$|^BenchmarkSetSync$|^BenchmarkSetOverwrite$|^BenchmarkGet$|^BenchmarkGetParallel$|^BenchmarkMem|Raw' \
        -benchmem -benchtime=1s -count=10 -timeout=40m ./bitcask/...

# Recovery + compaction
go test -bench='^BenchmarkOpen|^BenchmarkMerge$' \
        -benchmem -benchtime=1s -count=5 -timeout=40m ./bitcask/...

# Space amplification tables
go test -run 'TestSpaceAmplification|TestDeleteSpaceCost' -v ./bitcask/...

# Correctness
go test ./bitcask/...
go test -race ./bitcask/...   # requires CGO_ENABLED=1 and a C toolchain
```

`-count` is not optional. A single repetition on this hardware is not a measurement.
