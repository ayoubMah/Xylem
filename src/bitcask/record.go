// Package bitcask implements a persistent, crash-safe key/value store based on
// the Bitcask storage model (Sheehy & Smith, Riak, 2010).
//
// The design has exactly two halves:
//
//   - ON DISK: one append-only log file. Nothing is ever modified in place.
//     Every Set and every Delete appends a brand-new record to the end.
//   - IN MEMORY: the "keydir", a map from key -> byte offset of that key's most
//     recent record in the log.
//
// Consequences of that shape:
//
//	write = one sequential append          (fast, and never damages old data)
//	read  = one map lookup + one disk seek (bounded, predictable)
//	cost  = every key must fit in RAM      (the price paid for the two above)
//
// This file defines the on-disk record: how a key/value pair becomes bytes, and
// how those bytes are validated on the way back.
package bitcask

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"os"
)

// ---------------------------------------------------------------------------
// ON-DISK RECORD LAYOUT
// ---------------------------------------------------------------------------
//
// All integers are BIG-ENDIAN. One endianness everywhere, no exceptions --
// mixing them is the most common corruption bug in hand-rolled binary formats.
//
//	[0:4]    CRC32     uint32 -- checksum of EVERY byte from offset 4 onward
//	[4:12]   Timestamp int64  -- Unix nanoseconds, when this record was written
//	[12:16]  KeyLen    uint32 -- length of Key, in bytes
//	[16:20]  ValLen    uint32 -- length of Value, in bytes (or TombstoneValLen)
//	[20            : 20+KeyLen]          Key
//	[20+KeyLen     : 20+KeyLen+ValLen]   Value
//
// The header is fixed at 20 bytes, so a reader can always read 20 bytes, learn
// how long the rest is, then read exactly that much. That is what
// "self-describing" means: from a record's first byte you can compute its last
// byte without consulting anything else.
const (
	crcOffset    = 0  // where the checksum lives
	tsOffset     = 4  // where the checksummed region STARTS
	keyLenOffset = 12 // .
	valLenOffset = 16 // .

	// HeaderSize is the fixed prefix before the key bytes begin.
	HeaderSize = 20
)

// TombstoneValLen is a sentinel stored in the ValLen field to mark a deletion.
//
// A delete cannot erase bytes -- the log is append-only -- so a delete appends a
// record meaning "as of this timestamp, this key is gone". That must be
// distinguishable from a legitimate empty value, Set(k, []byte{}), so we reserve
// the one length a real value can never have: 2^32-1, which is far above
// MaxValueSize and therefore unambiguous.
//
// A tombstone record carries its key but writes ZERO value bytes to disk.
const TombstoneValLen = ^uint32(0) // 0xFFFFFFFF

// Sanity limits. These exist for a specific failure: if KeyLen is corrupted to
// 0xFFFFFFF0, a reader that trusts it will try to allocate ~4 GiB and take the
// process down. A length field read off disk is UNTRUSTED INPUT until the CRC
// has verified it -- but the length must be bounded BEFORE we can afford to read
// the bytes the CRC covers. Hence the order: bound it, read it, verify it.
const (
	MaxKeySize   = 64 << 10 // 64 KiB
	MaxValueSize = 16 << 20 // 16 MiB
)

// Sentinel errors. Callers compare with errors.Is, never by string.
var (
	// ErrKeyNotFound means the keydir holds no live entry for this key.
	ErrKeyNotFound = errors.New("bitcask: key not found")

	// ErrEmptyKey rejects a zero-length key at write time, which makes a zero
	// KeyLen on disk always a corruption signal and never legitimate data.
	ErrEmptyKey = errors.New("bitcask: key must not be empty")

	// ErrKeyTooLarge and ErrValueTooLarge enforce the sanity limits above.
	ErrKeyTooLarge   = errors.New("bitcask: key exceeds MaxKeySize")
	ErrValueTooLarge = errors.New("bitcask: value exceeds MaxValueSize")

	// ErrCorrupt means every byte was present but the checksum disagreed.
	ErrCorrupt = errors.New("bitcask: corrupt record (checksum mismatch)")

	// ErrShortRecord means the record was cut off -- the process died
	// mid-append. This is the EXPECTED way a log ends after a crash, so during
	// recovery it is a normal stop condition, not a failure.
	ErrShortRecord = errors.New("bitcask: truncated record")
)

// Record is the in-memory form of one log entry.
type Record struct {
	Ts        int64  // Unix nanoseconds
	Key       []byte // never empty
	Value     []byte // nil when Tombstone is true
	Tombstone bool   // true => this record deletes Key
}

// EncodeRecord serialises r into one contiguous byte slice, ready to append.
//
// One slice, one write call. That matters: the fewer write syscalls per record,
// the narrower the window in which a crash can tear a record in half.
func EncodeRecord(r *Record) ([]byte, error) {
	// --- validate before touching any bytes ---------------------------------
	if len(r.Key) == 0 {
		return nil, ErrEmptyKey
	}
	if len(r.Key) > MaxKeySize {
		return nil, ErrKeyTooLarge
	}
	if !r.Tombstone && len(r.Value) > MaxValueSize {
		return nil, ErrValueTooLarge
	}

	// --- decide what the ValLen field says, and how many value bytes are
	//     actually written. For a tombstone these differ: the field reads
	//     0xFFFFFFFF, but zero value bytes reach the disk. --------------------
	valLen := uint32(len(r.Value))
	body := r.Value
	if r.Tombstone {
		valLen = TombstoneValLen
		body = nil
	}

	// --- allocate the exact size, once. No append, no growth, no realloc.
	buf := make([]byte, HeaderSize+len(r.Key)+len(body))

	// --- header. PutUintNN writes into the slice starting at the given index,
	//     and each call knows its own width (64 -> 8 bytes, 32 -> 4 bytes).
	//
	//     uint64(r.Ts) reinterprets the bit pattern of a signed int64; two's
	//     complement is preserved exactly, so even a negative timestamp would
	//     round-trip.
	binary.BigEndian.PutUint64(buf[tsOffset:], uint64(r.Ts))
	binary.BigEndian.PutUint32(buf[keyLenOffset:], uint32(len(r.Key)))
	binary.BigEndian.PutUint32(buf[valLenOffset:], valLen)

	// --- body: key straight after the header, value straight after the key.
	copy(buf[HeaderSize:], r.Key)
	copy(buf[HeaderSize+len(r.Key):], body)

	// --- checksum LAST, over everything after itself ------------------------
	//
	// THE central design decision of this format. The CRC covers buf[4:] --
	// timestamp + KeyLen + ValLen + Key + Value. It deliberately covers the
	// LENGTH FIELDS and not just the payload: a flipped bit in KeyLen is the
	// most dangerous corruption possible, because it does not damage one
	// record, it desynchronises the reader from the record boundary and turns
	// the entire remainder of the log into garbage. Checksumming only the
	// payload would leave that failure completely undetectable.
	//
	// The CRC cannot cover itself -- it would have to be known before it is
	// computed -- which is precisely why it sits at offset 0 and the covered
	// region begins at 4.
	crc := crc32.ChecksumIEEE(buf[tsOffset:])
	binary.BigEndian.PutUint32(buf[crcOffset:], crc)

	return buf, nil
}

// DecodeRecord parses and VERIFIES one record from a complete buffer.
//
// It returns ErrShortRecord when buf does not contain the whole record, and
// ErrCorrupt when it does but the checksum disagrees. Those are different facts
// about the world and callers treat them differently.
func DecodeRecord(buf []byte) (*Record, error) {
	// Not even a full header present.
	if len(buf) < HeaderSize {
		return nil, ErrShortRecord
	}

	keyLen := binary.BigEndian.Uint32(buf[keyLenOffset:])
	valLen := binary.BigEndian.Uint32(buf[valLenOffset:])
	tombstone := valLen == TombstoneValLen

	// BOUNDS CHECK BEFORE ARITHMETIC. These lengths came off disk and are not
	// trusted yet. Zero key length is impossible by construction (ErrEmptyKey),
	// so seeing one means the bytes are garbage.
	if keyLen == 0 || keyLen > MaxKeySize {
		return nil, ErrCorrupt
	}
	if !tombstone && valLen > MaxValueSize {
		return nil, ErrCorrupt
	}

	// How many value bytes are physically present. Zero for a tombstone.
	onDiskVal := 0
	if !tombstone {
		onDiskVal = int(valLen)
	}

	total := HeaderSize + int(keyLen) + onDiskVal
	if len(buf) < total {
		// The header was intact and plausible but the body is cut off: a torn
		// write. The process died between header and body reaching the OS.
		return nil, ErrShortRecord
	}

	// --- verify before trusting ANY of it -----------------------------------
	want := binary.BigEndian.Uint32(buf[crcOffset:])
	got := crc32.ChecksumIEEE(buf[tsOffset:total])
	if got != want {
		return nil, ErrCorrupt
	}

	// --- only now is it safe to hand these bytes back as data ---------------
	//
	// Key and value are COPIED out of buf rather than sub-sliced. Sub-slicing
	// would pin the whole buffer in memory and, worse, hand the caller a window
	// into storage we may reuse. Copying is the boring, correct choice.
	rec := &Record{
		Ts:        int64(binary.BigEndian.Uint64(buf[tsOffset:])),
		Key:       make([]byte, keyLen),
		Tombstone: tombstone,
	}
	copy(rec.Key, buf[HeaderSize:HeaderSize+int(keyLen)])

	if !tombstone {
		rec.Value = make([]byte, onDiskVal)
		copy(rec.Value, buf[HeaderSize+int(keyLen):total])
	}

	return rec, nil
}

// WriteRecord appends the encoded form of r to f at the given offset and
// reports how many bytes were written.
//
// It uses WriteAt with an explicit offset rather than O_APPEND because the
// caller must know exactly where the record landed -- that offset is what goes
// into the keydir.
func WriteRecord(f *os.File, offset int64, r *Record) (int64, error) {
	buf, err := EncodeRecord(r)
	if err != nil {
		return 0, err
	}
	n, err := f.WriteAt(buf, offset)
	// n is what actually reached the file. On a short write it is less than
	// len(buf) and err is non-nil; returning the true count either way lets the
	// caller reason about the file's real length.
	return int64(n), err
}

// ReadRecord reads exactly one record starting at offset.
//
// It returns the record and its total on-disk size, so a scanner advances to
// the next record with offset += n.
//
// This is a TWO-READ design: read the fixed 20-byte header, learn the body
// length, then read the whole record. Simple and obviously correct. A
// production engine would read a larger speculative chunk to save the second
// syscall, or mmap the file and skip the syscalls altogether.
func ReadRecord(f *os.File, offset int64) (*Record, int64, error) {
	header := make([]byte, HeaderSize)
	if _, err := f.ReadAt(header, offset); err != nil {
		// ReadAt returns io.EOF when it cannot fill the buffer completely. At
		// the end of a healthy log, that is simply where the data stops.
		if errors.Is(err, io.EOF) {
			return nil, 0, ErrShortRecord
		}
		return nil, 0, err
	}

	keyLen := binary.BigEndian.Uint32(header[keyLenOffset:])
	valLen := binary.BigEndian.Uint32(header[valLenOffset:])
	tombstone := valLen == TombstoneValLen

	// The same untrusted-length guard as DecodeRecord, applied BEFORE an
	// allocation is sized from these numbers.
	if keyLen == 0 || keyLen > MaxKeySize {
		return nil, 0, ErrCorrupt
	}
	if !tombstone && valLen > MaxValueSize {
		return nil, 0, ErrCorrupt
	}

	onDiskVal := int64(0)
	if !tombstone {
		onDiskVal = int64(valLen)
	}
	total := int64(HeaderSize) + int64(keyLen) + onDiskVal

	buf := make([]byte, total)
	if _, err := f.ReadAt(buf, offset); err != nil {
		if errors.Is(err, io.EOF) {
			// Header present, body truncated: a torn write.
			return nil, 0, ErrShortRecord
		}
		return nil, 0, err
	}

	rec, err := DecodeRecord(buf)
	if err != nil {
		return nil, 0, err
	}
	return rec, total, nil
}
