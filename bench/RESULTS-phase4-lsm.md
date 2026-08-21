# Phase 4 — LSM-Tree: measured results

**Date:** 2026-08-21
**Package:** `src/lsm`
**Raw output:** `bench/raw/2026-08-21-lsm-bench.txt`

---

## Methodology

Identical to the Phase 2 suite, and for the same reason. The host is a 13th Gen
Intel i5-1335U — a 15 W mobile CPU — running Windows with real-time antivirus on
the I/O path. Absolute nanosecond figures are **indicative only**; several
benchmarks are visibly bimodal and drift with thermal state.

Every claim below is therefore a **ratio between two things measured in the same
process, in the same run, against the same workload**, reported as a **median of
5** with the full spread available in the raw file. Ratios survive a noisy host.
Absolute numbers do not, and are not load-bearing for any conclusion here.

`b.N` is pinned with `-benchtime Nx` rather than letting the framework choose,
so every variant in a comparison does the same amount of work.

---

## What was built

| Component | File | Status |
|---|---|---|
| Skiplist memtable | `memtable.go` | ✅ built + tested |
| SSTable (dense index, footer-last) | `sstable.go` | ✅ built + tested |
| Bloom filter (10 bits/key, k=7) | `bloom.go` | ✅ built + tested |
| DB: flush trigger, multi-table read path | `db.go` | ✅ built + tested |
| Write-ahead log + crash recovery | `wal.go` | ✅ built + tested |
| k-way merge compaction | `compact.go` | ✅ built + tested |

**25 tests, all green.** `go build`, `go vet`, `gofmt` clean.
Roadmap Sessions 9, 10 and 11 checklists are complete.

⚠️ `go test -race` has **not** been run — no C toolchain on this machine, same
outstanding item as Phase 2.

---

## The five findings

### F1 — Sequential vs random writes: 3.9×, and only when fsync is off

Measured at the **syscall floor**, with no engine around it: a 25 MiB file of
128-byte slots, writing the same bytes either to a random slot (a B-Tree's
in-place update) or to the end (a log's append).

| Pattern | Median ns/op | Ratio |
|---|---:|---:|
| Random in-place `WriteAt` | 45,791 | **3.91×** |
| Sequential append `WriteAt` | 11,711 | 1.00× |

This is the textbook claim, and it holds — **but only in the buffered case, and
that qualification turns out to be the whole story.** See F2.

Note this is a *lower bound* on a B-Tree's cost: the benchmark has no nodes, no
splits, no rebalancing. A real on-disk B-Tree can only be slower.

### F2 — With fsync per write, the access pattern stops mattering entirely

| Pattern | Median ns/op | Ratio |
|---|---:|---:|
| Random in-place + `Sync()` | 1,476,702 | 1.00× |
| Sequential append + `Sync()` | 1,514,579 | **1.03×** |

**The 3.91× advantage disappears completely.** Sequential is, if anything,
marginally slower — well inside noise.

The reason is the same mechanism Phase 2's F1 identified from the other side.
Without fsync both writes land in the page cache, and the page cache does not
care where in a file a page lives. The cost of randomness is paid at
*writeback*, when the kernel can coalesce adjacent dirty pages into one large
I/O but cannot merge scattered ones. Force a flush after every single write and
there is nothing to coalesce in either case — every write becomes one
device-level round trip, and the pattern is invisible underneath it.

So the page cache is not noise in this measurement. **It is the thing that
creates the effect, and fsync is what destroys it.**

### F3 — The LSM's real win is batching, not sequentiality: **246×**

The LSM's actual write pattern is neither of the above. It buffers a memtable in
RAM and flushes it as one sequential run with **one** fsync at the end.

| Pattern | Median ns/op | vs per-write sync |
|---|---:|---:|
| Sequential append + fsync **per write** | 1,514,579 | 1.00× |
| Sequential append + fsync **per 1,000** | 6,158 | **245.9×** |

**This is the load-bearing result of Phase 4.** The LSM is not fast because
appending is fast — F2 shows appending is not meaningfully fast once durability
is demanded. It is fast because **it amortises the durability cost across an
entire memtable.**

It also puts Phase 2's F1 in its proper place. Phase 2 measured fsync-per-write
at 96.4× and called it the durability/latency dial. Phase 4 shows the dial has a
third position that neither Phase 1 nor Phase 2 could reach: *batch, then sync
once*. Bitcask cannot use it, because its log **is** the database and every
record must be durable where it lands. An LSM can, because its log is only a
safety net and the real data structure is assembled in memory first.

### F4 — Bitcask is already write-optimal; the LSM does not beat it on latency

This contradicts the Phase 4 roadmap's ship checkpoint, which predicted "LSM
shows faster write ops/sec". Measured, in the same process:

| Operation | Bitcask | LSM | Ratio |
|---|---:|---:|---:|
| Write (unique keys) | 9,923 ns | 11,619 ns | LSM **1.17× slower** |
| Overwrite (1k hot keys) | 8,496 ns | 11,109 ns | LSM **1.31× slower** |
| Read (50k corpus, 1 table) | 29,918 ns | 34,274 ns | LSM **1.15× slower** |
| Allocations per write | 3 (256 B) | 7 (615 B) | LSM 2.3× more |

The prediction was wrong because it compared against the wrong baseline.
**Bitcask is already a log-structured engine** — it already converts every write
into a sequential append, which is exactly the transformation the LSM exists to
perform. On top of that same append the LSM additionally maintains a skiplist,
and pays O(log n) ordered insertion where Bitcask's hash keydir pays O(1).

The LSM's advantage over an in-place B-Tree is real and is F1/F3. Its advantage
over Bitcask is **not throughput at all** — it is F5, plus ordered iteration,
which a hash keydir cannot provide at any price.

### F5 — Space: constant under overwrite, where Bitcask is linear

Phase 2 measured Bitcask's space amplification as **exactly** the overwrite
factor. The same workload against the LSM:

| Overwrites | WAL bytes | SSTable bytes after flush |
|---:|---:|---:|
| 1× | 59,500 | **69,666** |
| 2× | 119,000 | **69,666** |
| 5× | 297,500 | **69,666** |
| 10× | 595,000 | **69,666** |
| 50× | 2,975,000 | **69,666** |

**The SSTable is byte-for-byte identical at every overwrite factor.** The
memtable absorbs repeat writes in place, so N writes to one key produce one
on-disk entry rather than N. The WAL grows linearly — it must, it records every
accepted mutation — but it is reset the instant a flush makes it redundant, so
it is a bounded transient rather than permanent bloat.

Compaction handles the cross-generation case: 10 generations of 200 keys
collapsed **10.00×**, from 198,910 B to 19,891 B.

---

## The mistake worth recording: the bloom filter buys nothing here

The bloom filter works exactly as designed in isolation — **0.803% false
positives** at 10 bits/key with k=7, against a theoretical ~1%, and zero false
negatives over 10,000 keys.

And in this engine it is **worthless**. Measured against a control with every
filter stripped out:

| Configuration | Median ns/op |
|---|---:|
| Miss over 16 tables, bloom filters on | 3,595 |
| Miss over 16 tables, **bloom filters off** | 3,680 |

**1.02×. Nothing.**

The cause is a design decision made earlier in the same session. This SSTable
uses a **dense index** — one in-memory entry per key. So the read path for a
missing key is:

```
bloom says "maybe"  ->  binary-search the index  ->  key absent  ->  return
                                                     (no disk read ever happens)
```

The index already answers "not in this table" from memory. There is no disk read
for the filter to save, so ~1 byte of RAM per key is being spent to skip a
binary search.

Bloom filters are not useless in general — they are essential in LevelDB and
RocksDB. But those engines use a **sparse index**, holding only the first key of
each ~4 KiB block. With a sparse index the index *cannot* answer "is this key
present", so a lookup must read and scan the candidate block, and the bloom
filter is what avoids that disk read. **The filter's value is created by the
sparse index, not by the LSM.**

This is the clearest instance in the whole engine of a component that is correct,
well-tested, textbook-faithful, and paying no rent — and it was only visible
because a control was measured. Two structures that are always described
together turn out to be coupled: one of them is what makes the other worth having.

---

## Read amplification, measured

A lookup for a key present in **no** table is an LSM's worst case: it cannot
stop early and must consult every table.

| Tables | Median ns/op |
|---:|---:|
| 1 | 307 |
| 4 | 1,440 |
| 16 | 3,595 |
| 32 | 22,426 |

Roughly linear in table count through 16 and super-linear by 32 (32 separate
index slices stop fitting cache comfortably). This is the **R** of the RUM
conjecture as a measured curve, and it is the direct cost of the cheap write
path — which is what compaction exists to pay back.

---

## For the write-up

The Future Work chapter's cost model is analytic. These are the numbers that let
it stop being purely analytic:

1. **F3 (246×)** is the sentence that carries the chapter. The LSM's advantage is
   *amortised durability*, not sequential I/O. That is a sharper and more
   defensible claim than the one the roadmap started with, and it is measured.
2. **F4 contradicts the roadmap's own prediction and should be reported as
   such.** Building it is what revealed that Bitcask was already write-optimal
   and that the intended comparison had the wrong baseline. A thesis that
   reports a falsified prediction is stronger than one that quietly drops it.
3. **F5 pairs directly with the Phase 2 space-amplification result** — the two
   tables belong side by side. Bitcask: amplification = overwrite factor. LSM:
   amplification = 1.00× at every overwrite factor.
4. **The bloom-filter finding is the best teaching result of the session.** It is
   a measured negative, it is counter-intuitive, and it explains *why* real
   engines pair sparse indexes with bloom filters — a connection that reading
   either component's description alone does not make.

Connects back to Phase 3's load-bearing paragraph. That one said sorted input is
the worst case for a B-Tree's space and the best case for a log. This phase adds
the write-cost half: **the B-Tree's problem was never the sort order, it was
having to write in place at all.**
