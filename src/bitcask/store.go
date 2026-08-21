package bitcask

import (
	"errors"
	"os"
	"sync"
	"time"
)

// Store is a crash-safe key/value store backed by a single append-only log.
//
// INVARIANTS -- everything below exists to keep these true:
//
//  1. keydir[k] is the offset of the MOST RECENT record for k.
//  2. tail is always the offset one byte past the last COMPLETE record, so an
//     append can never land on top of live data.
//  3. A key is in keydir only if its record is already on the file. Disk first,
//     memory second -- never the other way round.
type Store struct {
	// One RWMutex, same reasoning as Phase 1: reads are the common case and
	// they only touch the map plus a positional read, so they can run
	// concurrently. Writes move `tail` and must be exclusive.
	mu sync.RWMutex

	path string
	f    *os.File

	// keydir: key -> byte offset of that key's newest record.
	// This is the whole reason a read costs one seek instead of a scan, and
	// also the reason RAM bounds how many keys the store can hold.
	keydir map[string]int64

	// tail is where the next record will be appended (== logical file length).
	tail int64

	// syncOnWrite calls fsync after every append. See Options.
	syncOnWrite bool

	closed bool
}

// Options configures Open.
type Options struct {
	// SyncOnWrite forces an fsync after every Set and Delete.
	//
	// This is the durability/latency dial, and it is worth being precise about
	// what it buys, because the distinction is almost always got wrong:
	//
	//   OFF: write(2) has returned, so the data is in the OS page cache. If the
	//        PROCESS dies -- panic, os.Exit, kill -9 -- the kernel still holds
	//        the data and writes it out. Nothing is lost.
	//   ON:  the data is on the physical device. Survives the machine losing
	//        power or the kernel panicking.
	//
	// So fsync is not what makes a process crash survivable. It is what makes a
	// POWER LOSS survivable, and it costs orders of magnitude more per write.
	SyncOnWrite bool
}

// Open opens (or creates) the log at path and rebuilds the keydir from it.
//
// This is the recovery path, and it is the entire crash story: there is no
// separate repair step, no journal to replay, no fsck. Restarting IS recovery.
// The log is scanned from byte 0; every valid record overwrites the previous
// keydir entry for its key, so replaying in file order naturally leaves the
// newest write winning.
//
// Scanning stops at the first record that is truncated or fails its checksum,
// and the file is then TRUNCATED to that point. That is deliberate: a half
// record at the tail is exactly what a crash leaves behind, and if it were left
// in place the next append would sit behind garbage and the log would be
// unreadable forever after.
func Open(path string, opts *Options) (*Store, error) {
	if opts == nil {
		opts = &Options{}
	}

	// O_CREATE|O_RDWR: create if absent, read and write. Note the absence of
	// O_APPEND -- offsets are tracked explicitly so each record's position is
	// known and can be stored in the keydir.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}

	s := &Store{
		path:        path,
		f:           f,
		keydir:      make(map[string]int64),
		syncOnWrite: opts.SyncOnWrite,
	}

	// --- replay ------------------------------------------------------------
	var offset int64
	for {
		rec, n, err := ReadRecord(f, offset)
		if err != nil {
			// These two are how a log legitimately ends after a crash:
			// ErrShortRecord = torn write, ErrCorrupt = bad checksum.
			// Anything else is a real I/O failure and must not be swallowed.
			if errors.Is(err, ErrShortRecord) || errors.Is(err, ErrCorrupt) {
				break
			}
			f.Close()
			return nil, err
		}

		if rec.Tombstone {
			// A tombstone means the key was deleted AT THIS POINT in the log.
			// Any earlier Set for it is now dead. A later Set would appear
			// further down and put it back -- order is what resolves this.
			delete(s.keydir, string(rec.Key))
		} else {
			s.keydir[string(rec.Key)] = offset
		}

		offset += n
	}

	// --- drop the torn tail -------------------------------------------------
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if fi.Size() > offset {
		if err := f.Truncate(offset); err != nil {
			f.Close()
			return nil, err
		}
	}

	s.tail = offset
	return s, nil
}

// Set stores value under key by appending a new record.
//
// The old record for this key is NOT touched. It stays on disk as garbage until
// Merge reclaims it -- that is the space amplification the append-only design
// trades for never risking live data during a write.
func (s *Store) Set(key, value []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return os.ErrClosed
	}

	rec := &Record{
		Ts:    time.Now().UnixNano(),
		Key:   key,
		Value: value,
	}

	// DISK FIRST. If this write fails, the keydir must not be left claiming a
	// key that is not on the file -- a later Get would then seek to an offset
	// holding nothing. Update memory only after the bytes are out.
	n, err := WriteRecord(s.f, s.tail, rec)
	if err != nil {
		return err
	}
	if s.syncOnWrite {
		if err := s.f.Sync(); err != nil {
			return err
		}
	}

	s.keydir[string(key)] = s.tail
	s.tail += n
	return nil
}

// Get returns the value stored under key.
//
// One map lookup for the offset, one positional read for the bytes. No scan, no
// index traversal, no matter how large the log has grown.
func (s *Store) Get(key []byte) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil, os.ErrClosed
	}

	offset, ok := s.keydir[string(key)]
	if !ok {
		return nil, ErrKeyNotFound
	}

	rec, _, err := ReadRecord(s.f, offset)
	if err != nil {
		return nil, err
	}

	// Defensive: a live keydir entry should never point at a tombstone, since
	// Delete removes the entry. If it does, an invariant has been broken and
	// reporting "not found" is the honest answer.
	if rec.Tombstone {
		return nil, ErrKeyNotFound
	}
	return rec.Value, nil
}

// Delete removes key by appending a tombstone record.
//
// Nothing is erased. The tombstone must be written and not merely dropped from
// the keydir, because the keydir is rebuilt from the log on the next Open -- an
// in-memory-only delete would silently resurrect the key after a restart.
func (s *Store) Delete(key []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return os.ErrClosed
	}

	if _, ok := s.keydir[string(key)]; !ok {
		return ErrKeyNotFound
	}

	rec := &Record{
		Ts:        time.Now().UnixNano(),
		Key:       key,
		Tombstone: true,
	}

	n, err := WriteRecord(s.f, s.tail, rec)
	if err != nil {
		return err
	}
	if s.syncOnWrite {
		if err := s.f.Sync(); err != nil {
			return err
		}
	}

	delete(s.keydir, string(key))
	s.tail += n
	return nil
}

// Merge compacts the log: it rewrites it keeping only the newest record for
// each live key, discarding superseded values and tombstones entirely.
//
// CRASH SAFETY comes from writing to a side file and finishing with a rename:
//
//	crash before the rename -> the original log is untouched and complete;
//	                           the .merge file is orphaned garbage.
//	crash after  the rename -> the new log is complete and correct.
//
// There is no in-between state in which the live file is half-rewritten,
// because rename is atomic with respect to other observers of the path. This is
// the same trick every editor uses to save a file without risking your work.
//
// KNOWN LIMITATION, worth stating rather than hiding: on POSIX, full durability
// of a rename also requires fsync'ing the parent DIRECTORY, which Go does not
// expose portably and which this implementation does not do. The rename is
// atomic, but on power loss the directory entry itself could in principle be
// lost. Correct for process crashes; not fully hardened for power loss.
func (s *Store) Merge() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return os.ErrClosed
	}

	tmpPath := s.path + ".merge"
	tmp, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}

	// cleanup runs on every failure path so a crashed merge never leaves the
	// side file behind to confuse the next one.
	cleanup := func() {
		tmp.Close()
		os.Remove(tmpPath)
	}

	newKeydir := make(map[string]int64, len(s.keydir))
	var newTail int64

	// Only live keys are visited. Everything else -- superseded versions,
	// tombstones, and the keys they buried -- is simply never copied, which is
	// how the space is reclaimed.
	for key, offset := range s.keydir {
		rec, _, err := ReadRecord(s.f, offset)
		if err != nil {
			cleanup()
			return err
		}
		if rec.Tombstone {
			continue
		}

		// The ORIGINAL timestamp is preserved. Merge is a physical
		// reorganisation, not a new write; rewriting Ts would make compaction
		// look like user activity and destroy the log's history.
		n, err := WriteRecord(tmp, newTail, rec)
		if err != nil {
			cleanup()
			return err
		}
		newKeydir[key] = newTail
		newTail += n
	}

	// Get the new file fully onto disk BEFORE it becomes the live one. Renaming
	// a file whose contents are still only in the page cache would be a way to
	// lose everything at once.
	if err := tmp.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}

	// Windows will not rename over an open file, so the old handle is released
	// first. This is the one genuinely dangerous instant in the whole engine.
	if err := s.f.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}

	if err := os.Rename(tmpPath, s.path); err != nil {
		// The rename failed, so the original file is still intact -- reopen it
		// and leave the store usable rather than half-dead.
		if f, reopenErr := os.OpenFile(s.path, os.O_CREATE|os.O_RDWR, 0o644); reopenErr == nil {
			s.f = f
		} else {
			s.closed = true
		}
		os.Remove(tmpPath)
		return err
	}

	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		s.closed = true
		return err
	}

	// Swap all three pieces of state together. Until this point the store still
	// described the old file; after it, the new one. There is no moment where
	// keydir refers to one file and f to another.
	s.f = f
	s.keydir = newKeydir
	s.tail = newTail
	return nil
}

// Sync flushes the OS page cache for this file to the physical device.
func (s *Store) Sync() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return os.ErrClosed
	}
	return s.f.Sync()
}

// Close flushes and closes the underlying file. The store is unusable after.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if err := s.f.Sync(); err != nil {
		s.f.Close()
		return err
	}
	return s.f.Close()
}

// Len reports how many live keys the store holds.
func (s *Store) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.keydir)
}

// Size reports the current on-disk length of the log in bytes. Comparing this
// before and after Merge is the space-amplification measurement.
func (s *Store) Size() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.tail
}

// Keys returns a snapshot of the live keys, in unspecified order.
func (s *Store) Keys() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	keys := make([]string, 0, len(s.keydir))
	for k := range s.keydir {
		keys = append(keys, k)
	}
	return keys
}
