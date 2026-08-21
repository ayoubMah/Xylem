package lsm

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"os"
	"sort"
)

// ---------------------------------------------------------------------------
// THE SSTABLE (Sorted String Table)
// ---------------------------------------------------------------------------
//
// An SSTable is a memtable that has been dumped to disk, and its defining
// property is not that it is sorted -- it is that it is IMMUTABLE. Once
// written, no byte of it ever changes. Everything the LSM can do cheaply
// follows from that one decision:
//
//   - The write is ONE sequential pass. No seeking, no read-modify-write, no
//     in-place update. This is the whole point of the LSM: the B-Tree's
//     expensive random write has been converted into a sequential one by
//     buffering in the memtable first.
//   - It needs no locking. Readers can hit it concurrently forever because
//     there is no writer to race with.
//   - It can be reasoned about as a value: compaction does not EDIT SSTables,
//     it reads several and writes a new one, then deletes the inputs.
//
// The bill for all of that lands on the read path, and this file is where it
// starts: a key is no longer in one known place. It may be in the memtable, or
// in any SSTable, and the answer is whichever copy is NEWEST.
//
// FILE LAYOUT
//
//	+---------------------+  offset 0
//	| data records        |  ascending by key, self-describing, CRC'd
//	+---------------------+  <- indexOffset
//	| index entries       |  one per record: key -> data offset
//	+---------------------+  <- bloomOffset
//	| bloom filter        |  (Session 10; zero-length until then)
//	+---------------------+
//	| footer (36 bytes)   |  fixed size, so it is read by seeking from the END
//	+---------------------+  EOF
//
// The footer is fixed-length and last, which is what makes the file readable at
// all: you cannot know where the index lives until you have read something, and
// the only offset you know without reading anything is the file's length. So
// the reader seeks to EOF-36, learns where everything else is, and jumps.
// Every real format does this -- Parquet, ORC, LevelDB's own table format.

const (
	// sstMagic marks the last 4 bytes of a well-formed table. Cheap insurance
	// against opening a truncated file, a half-written flush, or something that
	// simply is not one of ours.
	sstMagic = 0x58594C4D // "XYLM"

	// footerSize is fixed forever. Changing it is a format break.
	footerSize = 36

	// sstHeaderSize is the fixed prefix of a data record:
	//   [0:4] crc32 over [4:end] | [4:8] keyLen | [8:12] valLen
	//
	// Same shape and the same reasoning as the Phase 2 bitcask record: the CRC
	// covers the LENGTH FIELDS, not just the payload, because a flipped bit in
	// a length does not corrupt one record -- it desynchronises the reader from
	// every record boundary that follows.
	//
	// The timestamp the bitcask record carries is absent here, and its absence
	// is the point: in a log, recency is a property of a RECORD and has to be
	// stored. In an LSM, recency is a property of the FILE -- a newer SSTable
	// is newer than an older one for every key it holds -- so per-record
	// timestamps would be 8 bytes of redundancy on every entry.
	sstHeaderSize = 12
)

var (
	// ErrNotSSTable means the magic did not match: not our format, or truncated.
	ErrNotSSTable = errors.New("lsm: not an sstable (bad magic)")

	// ErrCorruptTable means the structure was intact but a checksum disagreed.
	ErrCorruptTable = errors.New("lsm: corrupt sstable")

	// ErrKeyNotFound is returned by DB.Get when no live value exists.
	ErrKeyNotFound = errors.New("lsm: key not found")
)

// ---------------------------------------------------------------------------
// WRITING
// ---------------------------------------------------------------------------

// WriteSSTable writes entries to path as an immutable table and returns the
// finished, opened table.
//
// entries MUST already be sorted ascending and hold each key at most once,
// which is exactly what MemTable.Scan produces. That precondition is what
// keeps this function a single forward pass with no buffering of the data
// section: bytes go out in the order they arrive.
func WriteSSTable(path string, entries []Entry) (*SSTable, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, err
	}
	// On any failure below, the half-written file is removed rather than left
	// on disk. A partial SSTable is worse than no SSTable: it has a plausible
	// name and no valid footer, so every future Open would trip over it.
	ok := false
	defer func() {
		if !ok {
			f.Close()
			os.Remove(path)
		}
	}()

	var (
		off   int64
		index = make([]indexEntry, 0, len(entries))
	)

	// --- data section: one sequential pass --------------------------------
	for i := range entries {
		e := &entries[i]
		if len(e.Key) == 0 {
			return nil, fmt.Errorf("lsm: empty key at index %d", i)
		}
		if i > 0 && bytes.Compare(entries[i-1].Key, e.Key) >= 0 {
			// Cheap assertion of the precondition. If this ever fires, the bug
			// is upstream in the memtable or the merge, and finding it here
			// beats finding it as a silently unreadable file.
			return nil, fmt.Errorf("lsm: entries not sorted at index %d: %q >= %q",
				i, entries[i-1].Key, e.Key)
		}

		buf := encodeSSTRecord(e)
		if _, err := f.WriteAt(buf, off); err != nil {
			return nil, err
		}
		index = append(index, indexEntry{key: e.Key, offset: off})
		off += int64(len(buf))
	}

	indexOffset := off

	// --- index section ------------------------------------------------------
	//
	// A DENSE index: one entry per key. That makes a lookup a binary search
	// over an in-memory slice plus exactly one disk read, which is the simplest
	// thing that can possibly work and is easy to reason about.
	//
	// It also has a cost worth naming, because it is the same cost Phase 2 paid
	// and it is the reason real LSMs do not do this: a dense index must fit in
	// RAM, so this design reintroduces Bitcask's "every key must fit in memory"
	// bound that the LSM was supposed to escape. LevelDB avoids it with a
	// SPARSE index -- data is grouped into ~4 KiB blocks and only the FIRST key
	// of each block is indexed, so the in-memory cost falls by the number of
	// keys per block, at the price of scanning one block per lookup.
	//
	// That is designed here and not built, and the Future Work chapter says so.
	indexBuf := encodeIndex(index)
	if _, err := f.WriteAt(indexBuf, off); err != nil {
		return nil, err
	}
	off += int64(len(indexBuf))

	// --- bloom filter section ----------------------------------------------
	bloomOffset := off
	bloom := newBloom(len(entries))
	for i := range entries {
		bloom.add(entries[i].Key)
	}
	bloomBuf := bloom.encode()
	if _, err := f.WriteAt(bloomBuf, off); err != nil {
		return nil, err
	}
	off += int64(len(bloomBuf))

	// --- footer, last ------------------------------------------------------
	footer := encodeFooter(footer{
		indexOffset: indexOffset,
		indexLen:    uint32(len(indexBuf)),
		bloomOffset: bloomOffset,
		bloomLen:    uint32(len(bloomBuf)),
		count:       uint32(len(entries)),
	})
	if _, err := f.WriteAt(footer, off); err != nil {
		return nil, err
	}

	// fsync ONCE, for the whole table, right at the end.
	//
	// Phase 2 measured what per-write fsync costs: 96.4x, ~48,000 writes/s down
	// to ~500. The LSM pays that price exactly once per FLUSH rather than once
	// per SET, and amortising it over an entire memtable is a large part of why
	// an LSM's write path is cheap. Durability of individual writes between
	// flushes is the WAL's job (Session 11), not this file's.
	if err := f.Sync(); err != nil {
		return nil, err
	}

	ok = true
	return &SSTable{
		path:  path,
		f:     f,
		index: index,
		bloom: bloom,
	}, nil
}

func encodeSSTRecord(e *Entry) []byte {
	valLen := uint32(len(e.Value))
	body := e.Value
	if e.Tombstone {
		// Same sentinel trick as the bitcask record: reserve the one length a
		// real value can never have, so a deletion is distinguishable from
		// Set(k, []byte{}) without spending a flag byte on every record.
		valLen = TombstoneValLen
		body = nil
	}

	buf := make([]byte, sstHeaderSize+len(e.Key)+len(body))
	binary.BigEndian.PutUint32(buf[4:], uint32(len(e.Key)))
	binary.BigEndian.PutUint32(buf[8:], valLen)
	copy(buf[sstHeaderSize:], e.Key)
	copy(buf[sstHeaderSize+len(e.Key):], body)
	binary.BigEndian.PutUint32(buf[0:], crc32.ChecksumIEEE(buf[4:]))
	return buf
}

// TombstoneValLen mirrors the bitcask package's sentinel: 2^32-1, far above any
// legal value length, so it can never collide with real data.
const TombstoneValLen = ^uint32(0)

type indexEntry struct {
	key    []byte
	offset int64
}

func encodeIndex(index []indexEntry) []byte {
	// Size it exactly, once, then fill. No append growth.
	total := 0
	for _, e := range index {
		total += 4 + len(e.key) + 8
	}
	buf := make([]byte, total)
	p := 0
	for _, e := range index {
		binary.BigEndian.PutUint32(buf[p:], uint32(len(e.key)))
		p += 4
		p += copy(buf[p:], e.key)
		binary.BigEndian.PutUint64(buf[p:], uint64(e.offset))
		p += 8
	}
	return buf
}

func decodeIndex(buf []byte, count int) ([]indexEntry, error) {
	index := make([]indexEntry, 0, count)
	p := 0
	for p < len(buf) {
		if p+4 > len(buf) {
			return nil, ErrCorruptTable
		}
		kl := int(binary.BigEndian.Uint32(buf[p:]))
		p += 4
		// Bound the length that came off disk BEFORE using it to slice.
		if kl <= 0 || p+kl+8 > len(buf) {
			return nil, ErrCorruptTable
		}
		key := make([]byte, kl)
		copy(key, buf[p:p+kl])
		p += kl
		off := int64(binary.BigEndian.Uint64(buf[p:]))
		p += 8
		index = append(index, indexEntry{key: key, offset: off})
	}
	if len(index) != count {
		return nil, ErrCorruptTable
	}
	return index, nil
}

type footer struct {
	indexOffset int64
	indexLen    uint32
	bloomOffset int64
	bloomLen    uint32
	count       uint32
}

func encodeFooter(ft footer) []byte {
	buf := make([]byte, footerSize)
	binary.BigEndian.PutUint64(buf[0:], uint64(ft.indexOffset))
	binary.BigEndian.PutUint32(buf[8:], ft.indexLen)
	binary.BigEndian.PutUint64(buf[12:], uint64(ft.bloomOffset))
	binary.BigEndian.PutUint32(buf[20:], ft.bloomLen)
	binary.BigEndian.PutUint32(buf[24:], ft.count)
	binary.BigEndian.PutUint32(buf[28:], crc32.ChecksumIEEE(buf[0:28]))
	binary.BigEndian.PutUint32(buf[32:], sstMagic)
	return buf
}

func decodeFooter(buf []byte) (footer, error) {
	if len(buf) != footerSize {
		return footer{}, ErrNotSSTable
	}
	if binary.BigEndian.Uint32(buf[32:]) != sstMagic {
		return footer{}, ErrNotSSTable
	}
	if crc32.ChecksumIEEE(buf[0:28]) != binary.BigEndian.Uint32(buf[28:]) {
		return footer{}, ErrCorruptTable
	}
	return footer{
		indexOffset: int64(binary.BigEndian.Uint64(buf[0:])),
		indexLen:    binary.BigEndian.Uint32(buf[8:]),
		bloomOffset: int64(binary.BigEndian.Uint64(buf[12:])),
		bloomLen:    binary.BigEndian.Uint32(buf[20:]),
		count:       binary.BigEndian.Uint32(buf[24:]),
	}, nil
}

// ---------------------------------------------------------------------------
// READING
// ---------------------------------------------------------------------------

// SSTable is an open, immutable, sorted table.
//
// It carries no mutex. That is not an oversight -- the file never changes, so
// there is nothing to synchronise. Immutability is what buys lock-free reads.
type SSTable struct {
	path  string
	f     *os.File
	index []indexEntry
	bloom *bloomFilter
}

// OpenSSTable opens an existing table and loads its index and bloom filter.
//
// Reading runs backwards: seek to EOF-36 for the footer, which names where
// everything else lives.
func OpenSSTable(path string) (*SSTable, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}

	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if st.Size() < footerSize {
		f.Close()
		return nil, ErrNotSSTable
	}

	fbuf := make([]byte, footerSize)
	if _, err := f.ReadAt(fbuf, st.Size()-footerSize); err != nil {
		f.Close()
		return nil, err
	}
	ft, err := decodeFooter(fbuf)
	if err != nil {
		f.Close()
		return nil, err
	}

	// Bound every offset against the real file size before trusting it.
	if ft.indexOffset < 0 || ft.indexOffset+int64(ft.indexLen) > st.Size() ||
		ft.bloomOffset < 0 || ft.bloomOffset+int64(ft.bloomLen) > st.Size() {
		f.Close()
		return nil, ErrCorruptTable
	}

	ibuf := make([]byte, ft.indexLen)
	if _, err := f.ReadAt(ibuf, ft.indexOffset); err != nil && ft.indexLen > 0 {
		f.Close()
		return nil, err
	}
	index, err := decodeIndex(ibuf, int(ft.count))
	if err != nil {
		f.Close()
		return nil, err
	}

	bbuf := make([]byte, ft.bloomLen)
	if ft.bloomLen > 0 {
		if _, err := f.ReadAt(bbuf, ft.bloomOffset); err != nil {
			f.Close()
			return nil, err
		}
	}
	bloom, err := decodeBloom(bbuf)
	if err != nil {
		f.Close()
		return nil, err
	}

	return &SSTable{path: path, f: f, index: index, bloom: bloom}, nil
}

// Len reports how many entries the table holds, tombstones included.
func (s *SSTable) Len() int { return len(s.index) }

// Path reports the file this table was read from.
func (s *SSTable) Path() string { return s.path }

// Close releases the file handle.
func (s *SSTable) Close() error { return s.f.Close() }

// Get looks key up in this table alone.
//
// The return is the same three-way answer as MemTable.Get, and for the same
// reason: "deleted here" and "not in this table" are different facts, and a
// caller that confuses them resurrects deleted keys.
func (s *SSTable) Get(key []byte) (value []byte, found bool, tombstone bool, err error) {
	// --- bloom filter: the cheap NO -----------------------------------------
	//
	// This is the single most valuable line in the read path. Without it, a
	// lookup for a key that does not exist must binary-search and then READ
	// FROM DISK in every table. The filter answers "definitely not here" from
	// memory, in nanoseconds, and lets the read skip the file entirely.
	if !s.bloom.mayContain(key) {
		return nil, false, false, nil
	}

	// --- binary search the in-memory index ---------------------------------
	i := sort.Search(len(s.index), func(i int) bool {
		return bytes.Compare(s.index[i].key, key) >= 0
	})
	if i == len(s.index) || !bytes.Equal(s.index[i].key, key) {
		// The bloom filter said "maybe" and was wrong. That is a FALSE
		// POSITIVE, and it is the filter working as designed, not a bug.
		return nil, false, false, nil
	}

	e, err := s.readAt(s.index[i].offset)
	if err != nil {
		return nil, false, false, err
	}
	if e.Tombstone {
		return nil, true, true, nil
	}
	return e.Value, true, false, nil
}

// readAt reads and verifies one data record at off.
func (s *SSTable) readAt(off int64) (Entry, error) {
	header := make([]byte, sstHeaderSize)
	if _, err := s.f.ReadAt(header, off); err != nil {
		return Entry{}, err
	}

	keyLen := binary.BigEndian.Uint32(header[4:])
	valLen := binary.BigEndian.Uint32(header[8:])
	tombstone := valLen == TombstoneValLen

	// Bound before allocating from a number that came off disk.
	if keyLen == 0 || keyLen > MaxKeySize {
		return Entry{}, ErrCorruptTable
	}
	onDisk := 0
	if !tombstone {
		if valLen > MaxValueSize {
			return Entry{}, ErrCorruptTable
		}
		onDisk = int(valLen)
	}

	total := sstHeaderSize + int(keyLen) + onDisk
	buf := make([]byte, total)
	if _, err := s.f.ReadAt(buf, off); err != nil {
		return Entry{}, err
	}
	if crc32.ChecksumIEEE(buf[4:]) != binary.BigEndian.Uint32(buf[0:]) {
		return Entry{}, ErrCorruptTable
	}

	// Copy out rather than sub-slice, so the caller never holds a window into
	// a buffer we may reuse. Third time this principle appears in the engine.
	e := Entry{
		Key:       make([]byte, keyLen),
		Tombstone: tombstone,
	}
	copy(e.Key, buf[sstHeaderSize:sstHeaderSize+int(keyLen)])
	if !tombstone {
		e.Value = make([]byte, onDisk)
		copy(e.Value, buf[sstHeaderSize+int(keyLen):total])
	}
	return e, nil
}

// Scan returns every entry in the table, in key order.
//
// This is what compaction consumes. It walks the index rather than the file,
// which costs one read per record -- fine at these sizes, and a block-based
// layout with a sequential reader is the obvious optimisation if it ever is not.
func (s *SSTable) Scan() ([]Entry, error) {
	out := make([]Entry, 0, len(s.index))
	for _, ie := range s.index {
		e, err := s.readAt(ie.offset)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}

// Sanity limits, mirroring the bitcask package.
const (
	MaxKeySize   = 64 << 10
	MaxValueSize = 16 << 20
)
