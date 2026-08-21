package lsm

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"os"
)

// ---------------------------------------------------------------------------
// THE WRITE-AHEAD LOG
// ---------------------------------------------------------------------------
//
// The memtable is in RAM, so between two flushes every accepted write exists
// nowhere else. A crash at that moment loses all of it -- potentially a whole
// MemTableSize of data the caller was told had succeeded.
//
// The WAL closes that hole, and it does it with the cheapest possible
// structure: an append-only log of exactly the mutations that went into the
// memtable, in the order they went in.
//
//	write path:  WAL append (sequential)  ->  memtable insert (RAM)
//	recovery:    load SSTables            ->  replay WAL into a fresh memtable
//
// It is worth noticing what this log is NOT. It is not a second copy of the
// database, it is not indexed, and it is never read except during recovery. It
// only ever has to answer one question -- "what happened since the last flush?"
// -- which is why it can be a dumb sequential file and why it can be thrown
// away wholesale the moment a flush makes its contents redundant.
//
// This is also the same file format as the Phase 2 bitcask log, deliberately:
// an append-only sequence of CRC'd, self-describing records, recovered by
// scanning from byte 0 and stopping at the first record that does not verify.
// The difference is lifetime. Bitcask's log IS the database and lives forever;
// this one is a safety net that is reset on every flush. Same bytes, opposite
// role -- which is the clearest illustration in the whole engine that a storage
// structure is defined by its access pattern, not its layout.

// ErrClosed is returned by operations on a closed DB.
var ErrClosed = errors.New("lsm: database is closed")

// Input validation errors, mirroring the bitcask package.
var (
	ErrEmptyKey      = errors.New("lsm: key must not be empty")
	ErrKeyTooLarge   = errors.New("lsm: key exceeds MaxKeySize")
	ErrValueTooLarge = errors.New("lsm: value exceeds MaxValueSize")
)

// WAL is an append-only log of pending mutations.
type WAL struct {
	f    *os.File
	tail int64
	sync bool
}

// OpenWAL opens or creates the log at path and replays whatever it holds.
//
// The returned entries are in write order, so a caller that applies them in
// sequence naturally ends with the newest value for every key -- no timestamps
// or sequence numbers required, because file order IS the order.
func OpenWAL(path string, sync bool) (*WAL, []Entry, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, nil, err
	}

	entries, tail, err := replayWAL(f)
	if err != nil {
		f.Close()
		return nil, nil, err
	}

	// TRUNCATE TO THE LAST GOOD RECORD.
	//
	// A crash mid-append leaves a torn record at the tail. If it were left in
	// place the next append would sit behind garbage and every future replay
	// would stop short of it, silently losing everything written afterwards.
	// Cutting the file back to the last verified boundary is what makes the log
	// safe to keep using rather than something to throw away and start over.
	if err := f.Truncate(tail); err != nil {
		f.Close()
		return nil, nil, err
	}

	return &WAL{f: f, tail: tail, sync: sync}, entries, nil
}

// replayWAL scans from byte 0 and returns every complete, verified record plus
// the offset one byte past the last of them.
func replayWAL(f *os.File) ([]Entry, int64, error) {
	st, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	size := st.Size()

	var (
		entries []Entry
		off     int64
	)

	for off < size {
		e, n, err := readWALRecord(f, off, size)
		if err != nil {
			// A short or corrupt record is the EXPECTED way a log ends after a
			// crash, not a failure. Stop here and treat everything before it as
			// the truth. Anything after it is unreachable by construction: the
			// caller was never told those writes succeeded, because the record
			// that would have proven it never landed intact.
			if errors.Is(err, errShortWAL) || errors.Is(err, ErrCorruptTable) {
				break
			}
			return nil, 0, err
		}
		entries = append(entries, e)
		off += n
	}

	return entries, off, nil
}

var errShortWAL = errors.New("lsm: truncated wal record")

// readWALRecord reads one record at off. limit is the file size, used to detect
// a body that runs past the end of the file.
func readWALRecord(f *os.File, off, limit int64) (Entry, int64, error) {
	if off+sstHeaderSize > limit {
		return Entry{}, 0, errShortWAL
	}

	header := make([]byte, sstHeaderSize)
	if _, err := f.ReadAt(header, off); err != nil {
		if errors.Is(err, io.EOF) {
			return Entry{}, 0, errShortWAL
		}
		return Entry{}, 0, err
	}

	keyLen := binary.BigEndian.Uint32(header[4:])
	valLen := binary.BigEndian.Uint32(header[8:])
	tombstone := valLen == TombstoneValLen

	// Bound the untrusted lengths BEFORE they size an allocation. A flipped bit
	// in keyLen is the corruption that matters: it does not damage one record,
	// it desynchronises every boundary after it.
	if keyLen == 0 || keyLen > MaxKeySize {
		return Entry{}, 0, ErrCorruptTable
	}
	onDisk := int64(0)
	if !tombstone {
		if valLen > MaxValueSize {
			return Entry{}, 0, ErrCorruptTable
		}
		onDisk = int64(valLen)
	}

	total := int64(sstHeaderSize) + int64(keyLen) + onDisk
	if off+total > limit {
		// Header intact and plausible, body cut off: a torn write.
		return Entry{}, 0, errShortWAL
	}

	buf := make([]byte, total)
	if _, err := f.ReadAt(buf, off); err != nil {
		if errors.Is(err, io.EOF) {
			return Entry{}, 0, errShortWAL
		}
		return Entry{}, 0, err
	}
	if crc32.ChecksumIEEE(buf[4:]) != binary.BigEndian.Uint32(buf[0:]) {
		return Entry{}, 0, ErrCorruptTable
	}

	e := Entry{Key: make([]byte, keyLen), Tombstone: tombstone}
	copy(e.Key, buf[sstHeaderSize:sstHeaderSize+int64(keyLen)])
	if !tombstone {
		e.Value = make([]byte, onDisk)
		copy(e.Value, buf[sstHeaderSize+int64(keyLen):total])
	}
	return e, total, nil
}

// Append writes one mutation to the log.
func (w *WAL) Append(e Entry) error {
	buf := encodeSSTRecord(&e)

	// WriteAt with an explicit offset rather than O_APPEND, so the tail is
	// always known exactly and a short write cannot leave it ambiguous.
	n, err := w.f.WriteAt(buf, w.tail)
	w.tail += int64(n)
	if err != nil {
		return err
	}

	if w.sync {
		// The 96.4x dial from Phase 2, and the ONLY place in the LSM where a
		// per-write fsync happens. SSTables are fsynced once per flush; this is
		// the per-operation durability cost, paid on a purely sequential append
		// -- the cheapest write a disk can be asked for.
		return w.f.Sync()
	}
	return nil
}

// Reset empties the log.
//
// Called only after a flush has fsynced an SSTable containing every mutation
// the log holds. At that instant the log's contents are redundant, and keeping
// them would mean replaying writes that are already durable.
func (w *WAL) Reset() error {
	if err := w.f.Truncate(0); err != nil {
		return err
	}
	w.tail = 0
	// Truncation is metadata. Without an fsync the file could still be its old
	// length after a power loss, and recovery would replay records the SSTable
	// already contains. Those replays are idempotent -- same key, same value --
	// so this is cheap insurance rather than a correctness requirement.
	return w.f.Sync()
}

// Size reports the log's current length in bytes.
func (w *WAL) Size() int64 { return w.tail }

// Close releases the file handle.
func (w *WAL) Close() error { return w.f.Close() }
