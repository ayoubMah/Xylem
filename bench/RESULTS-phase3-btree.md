# Xylem — Phase 3 Benchmark Results (Session 6: in-memory B-Tree)

**Recorded:** 2026-08-21
**Source:** `src/btree/btree.go`, `src/btree/validate.go`, `src/btree/height_test.go`
**Scope:** in-memory only. No pages, no disk, no serialisation — that is Session 7.

Setup as in `RESULTS.md` §1 (i5-1335U, Go 1.26.2, Windows 11). Unlike the Phase 2 numbers, these
involve **no syscalls**, so run-to-run variance is small and the absolute figures are meaningful.

---

## 1. Height and fill: sequential vs random insertion

B-Tree with `t=2` (nodes hold 1–3 keys), against a naive unbalanced BST on identical input.

| n | Order | B-Tree height | B-Tree nodes | Fill | log₂(n) | **BST height** |
|---:|---|---:|---:|---:|---:|---:|
| 1,000 | sequential | 9 | 992 | 33.6 % | 10.0 | **1,000** |
| 1,000 | random | 8 | 575 | 58.0 % | 10.0 | 22 |
| 10,000 | sequential | 13 | 9,992 | 33.4 % | 13.3 | **10,000** |
| 10,000 | random | 11 | 5,653 | 59.0 % | 13.3 | 32 |
| 100,000 | sequential | 16 | 99,990 | 33.3 % | 16.6 | **100,000** |
| 100,000 | random | 13 | 57,033 | 58.4 % | 16.6 | 39 |

### F1 — the B-Tree is height-balanced regardless of insertion order; the BST is not

Fed ascending keys, the naive BST degenerates into a linked list: **height = n, exactly.** Every
insertion goes right, and a lookup becomes a linear scan.

The B-Tree, on the same input, reaches height 16 for 100,000 keys — against 13 for random input.
A difference of three levels, not a difference of five orders of magnitude.

The mechanism is structural rather than lucky. **A B-Tree only ever grows at the root.** Every
split below the root pushes its median into an existing parent, leaving the height unchanged; only
a full root creates a new level, and that lifts every leaf simultaneously. There is no sequence of
insertions that can put one leaf deeper than another, because depth is never added at the bottom.

### F2 — height-balanced is not space-balanced, and that is the non-obvious result

Fill factor is **33.3 %** for sequential input against **58.4 %** for random — and it is
suspiciously exactly one third.

It is not a coincidence. Ascending keys always split the same node, the rightmost one, and a split
leaves the left half with exactly `t-1` keys. At `t=2` that is 1 key in a node that holds 3. The
left half is then never touched again, because every subsequent key is larger. Sequential insertion
therefore strands one-third-full nodes behind it, permanently.

The cost is not height, it is **pages**: 99,990 nodes against 57,033 for the same 100,000 keys —
**1.75× the storage for identical data.** In memory that is wasted RAM. On disk, where one node is
one page, it is 1.75× the I/O for any scan, forever, unless the tree is rebuilt.

This is why real databases bulk-load sorted data bottom-up instead of inserting it key by key, and
why `REINDEX` exists. Sorted input — the most natural way to load a dataset — is the worst case for
space in a B-Tree, while being the best case for a log.

---

## 2. Why `t=2` is a teaching choice, not a design choice

100,000 random keys, varying the minimum degree.

| t | Max keys/node | Height | Nodes | Fill | Page reads per lookup |
|---:|---:|---:|---:|---:|---:|
| 2 | 3 | 13 | 56,846 | 58.6 % | 13 |
| 4 | 7 | 7 | 21,623 | 66.1 % | 7 |
| 8 | 15 | 5 | 9,774 | 68.2 % | 5 |
| 32 | 63 | 3 | 2,320 | 68.4 % | 3 |
| 128 | 255 | 3 | 552 | 71.0 % | 3 |
| 256 | 511 | 2 | 257 | 76.1 % | 2 |

Height falls as log_t(n), and **on disk the height *is* the read amplification** — one page read per
level. Going from `t=2` to `t=256` turns a 13-read lookup into a 2-read lookup, a **6.5× reduction
in I/O per query**, and shrinks the index from 56,846 nodes to 257.

Fill also improves with degree (58.6 % → 76.1 %), because a larger node leaves proportionally more
keys behind on each split.

`t=2` is used in Session 6 for the opposite reason: it is the smallest legal degree, so splits
happen constantly and any error in the split logic surfaces within a few insertions rather than
hiding until the tree is large. Session 7 picks `t` so that one node fills exactly one 4 KiB page.

---

## 3. Benchmarks

Medians over 5 repetitions, 100,000 keys resident.

| Benchmark | Median | Height | B/op | allocs/op |
|---|---:|---:|---:|---:|
| `Search` t=2 | 472.1 ns | 13 | 0 | 0 |
| `Search` t=32 | 101.7 ns | 3 | 0 | 0 |
| `Search` t=128 | 90.68 ns | 3 | 0 | 0 |
| `InsertSequential` t=32 | 47.92 ns | — | 27 | 0 |
| `InsertRandom` t=32 | 396.6 ns | — | 19 | 0 |

### F3 — lookups allocate nothing

`Search` is **0 B/op, 0 allocs/op** at every degree: it is a pointer walk with a binary search at
each node and no intermediate structure. This matters for Session 7, where the same property is
what will allow reads to be served straight out of a buffer-pool page without copying.

### F4 — raising the degree pays until it doesn't

`t=2` → `t=32` is a **4.6× speedup** (472.1 ns → 101.7 ns), tracking the height drop from 13 to 3.

`t=32` → `t=128` is nearly flat (101.7 ns → 90.68 ns) despite a 4× wider node, because both trees
are height 3: the levels saved are zero, and the extra width is paid back as a longer binary search
within each node. In memory, the returns stop as soon as the height stops falling.

**On disk this reverses**, and that is the point worth carrying into Session 7. Here a level costs a
pointer dereference, so flattening the tree buys little. There a level costs a page read measured in
microseconds — Phase 2 measured a single 132-byte positional read at 13.75 µs on this machine — so
the same flattening is worth roughly four orders of magnitude more.

### F5 — sequential insertion is 8.3× faster than random

47.92 ns against 396.6 ns. Ascending keys touch only the rightmost path, which stays in L1 cache;
random keys touch a different root-to-leaf path every time and miss at every level.

Note the direction: sequential insertion is much **faster** and much **worse-packed** (§1, F2). The
input pattern that is cheapest to insert is the one that wastes the most space.

---

## 4. Correctness

`Validate()` checks all five B-Tree invariants — key ordering, node arity, key-count bounds, equal
leaf depth, and the separator property — and is called throughout the tests, including after every
insertion in the small cases and every 250 in the randomised ones.

This exists because a broken B-Tree does not announce itself. A mis-handled split still answers most
queries correctly, since descent usually happens to take the right branch, and still reports a
plausible height. It fails only for the keys that landed on the wrong side of a lost separator.
Checking invariants catches that on the insertion that causes it, and names the invariant.

Tests passing: empty tree, small insert/search, duplicate rejection, root-split height growth,
5,000 randomised operations mirrored against a reference `map` at t ∈ {2, 3, 8, 64}, sorted-key
iteration, and 10,000 ascending insertions checked for degeneration.

## 5. Reproduction

```sh
cd src
go test ./btree/...
go test -run 'TestHeightSequentialVsRandom|TestHeightAcrossDegrees' -v ./btree/...
go test -bench=. -benchmem -count=5 ./btree/...
```
