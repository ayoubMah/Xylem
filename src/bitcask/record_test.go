package bitcask

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestEncodeDecodeRoundTrip is the table-driven base case: whatever goes in
// must come back out byte-identical.
func TestEncodeDecodeRoundTrip(t *testing.T) {
	tests := []struct {
		name      string
		key       []byte
		value     []byte
		tombstone bool
	}{
		{"simple", []byte("user:42"), []byte("ayoub"), false},
		{"empty value", []byte("k"), []byte{}, false},
		{"binary value", []byte("bin"), []byte{0x00, 0xFF, 0x00, 0xFF}, false},
		{"key with nul", []byte("a\x00b"), []byte("v"), false},
		{"large value", []byte("big"), bytes.Repeat([]byte("x"), 100000), false},
		{"max size key", bytes.Repeat([]byte("k"), MaxKeySize), []byte("v"), false},
		{"tombstone", []byte("gone"), nil, true},
		{"utf8 key", []byte("clé—unicode"), []byte("valeur"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := &Record{Ts: 1755000000000000000, Key: tt.key, Value: tt.value, Tombstone: tt.tombstone}

			buf, err := EncodeRecord(in)
			if err != nil {
				t.Fatalf("EncodeRecord: %v", err)
			}

			out, err := DecodeRecord(buf)
			if err != nil {
				t.Fatalf("DecodeRecord: %v", err)
			}

			if out.Ts != in.Ts {
				t.Errorf("Ts = %d, want %d", out.Ts, in.Ts)
			}
			if !bytes.Equal(out.Key, in.Key) {
				t.Errorf("Key = %q, want %q", out.Key, in.Key)
			}
			if out.Tombstone != in.Tombstone {
				t.Errorf("Tombstone = %v, want %v", out.Tombstone, in.Tombstone)
			}
			if !tt.tombstone && !bytes.Equal(out.Value, tt.value) {
				t.Errorf("Value = %q, want %q", out.Value, tt.value)
			}
		})
	}
}

// TestEncodeRejectsBadInput checks the guards fire before anything is written.
func TestEncodeRejectsBadInput(t *testing.T) {
	tests := []struct {
		name string
		rec  *Record
		want error
	}{
		{"empty key", &Record{Key: nil, Value: []byte("v")}, ErrEmptyKey},
		{"key too large", &Record{Key: bytes.Repeat([]byte("k"), MaxKeySize+1), Value: []byte("v")}, ErrKeyTooLarge},
		{"value too large", &Record{Key: []byte("k"), Value: make([]byte, MaxValueSize+1)}, ErrValueTooLarge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := EncodeRecord(tt.rec); !errors.Is(err, tt.want) {
				t.Errorf("err = %v, want %v", err, tt.want)
			}
		})
	}
}

// TestDecodeDetectsCorruption flips one bit in each region of the record and
// requires the checksum to catch it. This is the test that proves the CRC
// covers the length fields and not merely the payload.
func TestDecodeDetectsCorruption(t *testing.T) {
	rec := &Record{Ts: 123, Key: []byte("user:42"), Value: []byte("ayoub")}
	clean, err := EncodeRecord(rec)
	if err != nil {
		t.Fatalf("EncodeRecord: %v", err)
	}

	// One byte inside each field, so every region of the layout is exercised.
	regions := map[string]int{
		"timestamp": tsOffset,
		"key length": keyLenOffset + 3,
		"value length": valLenOffset + 3,
		"key bytes": HeaderSize,
		"value bytes": HeaderSize + len(rec.Key),
	}

	for name, pos := range regions {
		t.Run(name, func(t *testing.T) {
			corrupt := make([]byte, len(clean))
			copy(corrupt, clean)
			corrupt[pos] ^= 0x01 // flip exactly one bit

			_, err := DecodeRecord(corrupt)
			// Either outcome is a correct detection: ErrCorrupt means the CRC
			// caught it, ErrShortRecord means a mangled length field made the
			// record claim to be longer than the buffer. Both refuse the data.
			if !errors.Is(err, ErrCorrupt) && !errors.Is(err, ErrShortRecord) {
				t.Errorf("bit flip in %s went undetected: err = %v", name, err)
			}
		})
	}
}

// TestDecodeRejectsAbsurdLengths is the allocation-bomb guard: a corrupted
// length field must not make the reader try to allocate gigabytes.
func TestDecodeRejectsAbsurdLengths(t *testing.T) {
	rec := &Record{Ts: 1, Key: []byte("k"), Value: []byte("v")}
	buf, _ := EncodeRecord(rec)

	// Overwrite KeyLen with 0xFFFFFFF0 without fixing the CRC.
	buf[keyLenOffset] = 0xFF
	buf[keyLenOffset+1] = 0xFF
	buf[keyLenOffset+2] = 0xFF
	buf[keyLenOffset+3] = 0xF0

	if _, err := DecodeRecord(buf); !errors.Is(err, ErrCorrupt) {
		t.Errorf("err = %v, want ErrCorrupt", err)
	}
}

// TestWriteReadRecordOnFile exercises the real file path and, critically, that
// the returned size lets a scanner walk record to record.
func TestWriteReadRecordOnFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "records.log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()

	const n = 1000
	offsets := make([]int64, n)
	var tail int64

	for i := 0; i < n; i++ {
		rec := &Record{
			Ts:    int64(i),
			Key:   []byte(fmt.Sprintf("key-%04d", i)),
			Value: []byte(fmt.Sprintf("value-%04d-%s", i, bytes.Repeat([]byte("p"), i%40))),
		}
		offsets[i] = tail
		written, err := WriteRecord(f, tail, rec)
		if err != nil {
			t.Fatalf("WriteRecord %d: %v", i, err)
		}
		tail += written
	}

	// Random access: every recorded offset yields its own record.
	for i := 0; i < n; i++ {
		rec, _, err := ReadRecord(f, offsets[i])
		if err != nil {
			t.Fatalf("ReadRecord %d: %v", i, err)
		}
		want := fmt.Sprintf("key-%04d", i)
		if string(rec.Key) != want {
			t.Fatalf("record %d: key = %q, want %q", i, rec.Key, want)
		}
	}

	// Sequential scan: walking with offset += size must visit all n and land
	// exactly on the end of the file.
	var count int
	var off int64
	for {
		_, size, err := ReadRecord(f, off)
		if err != nil {
			break
		}
		count++
		off += size
	}
	if count != n {
		t.Errorf("scan visited %d records, want %d", count, n)
	}
	if off != tail {
		t.Errorf("scan ended at %d, want %d", off, tail)
	}
}

// TestTruncatedTailStopsCleanly is the torn-write test: cut the file in the
// middle of the last record and require the scan to stop without panicking,
// keeping every complete record before it.
func TestTruncatedTailStopsCleanly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "torn.log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()

	const n = 100
	var tail int64
	for i := 0; i < n; i++ {
		rec := &Record{Ts: int64(i), Key: []byte(fmt.Sprintf("k%03d", i)), Value: []byte("some value here")}
		written, err := WriteRecord(f, tail, rec)
		if err != nil {
			t.Fatalf("WriteRecord: %v", err)
		}
		tail += written
	}

	// Chop 7 bytes off: the last record now has a valid-looking header but a
	// body that stops early -- precisely what a crash mid-append leaves.
	if err := f.Truncate(tail - 7); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	var count int
	var off int64
	var lastErr error
	for {
		_, size, err := ReadRecord(f, off)
		if err != nil {
			lastErr = err
			break
		}
		count++
		off += size
	}

	if count != n-1 {
		t.Errorf("recovered %d records, want %d", count, n-1)
	}
	if !errors.Is(lastErr, ErrShortRecord) && !errors.Is(lastErr, ErrCorrupt) {
		t.Errorf("stopped with %v, want ErrShortRecord or ErrCorrupt", lastErr)
	}
}
