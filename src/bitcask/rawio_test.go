package bitcask

import (
	"os"
	"path/filepath"
	"testing"
)

// ---------------------------------------------------------------------------
// RAW I/O BASELINE -- how much of the engine's latency is the ENGINE?
// ---------------------------------------------------------------------------
//
// The Set/Get benchmarks measure the storage engine. These measure the floor
// underneath it: a bare os.File positional read or write of exactly one
// record's worth of bytes, with no encoding, no checksum, no keydir and no
// lock. Whatever the engine costs ABOVE this number is the part the
// implementation is responsible for; whatever it costs BELOW it is the
// platform.
//
// Without this baseline, any statement in the thesis of the form "a read costs
// N microseconds" is unattributable -- the reader cannot tell an inefficient
// engine from an expensive syscall.

const rawRecordSize = HeaderSize + 12 + benchValSize // 132 B, one benchmark record

func rawFile(b *testing.B, prefill int64) *os.File {
	b.Helper()
	path := filepath.Join(b.TempDir(), "raw.bin")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		b.Fatalf("OpenFile: %v", err)
	}
	if prefill > 0 {
		buf := make([]byte, prefill)
		if _, err := f.WriteAt(buf, 0); err != nil {
			b.Fatalf("prefill: %v", err)
		}
		if err := f.Sync(); err != nil {
			b.Fatalf("Sync: %v", err)
		}
	}
	b.Cleanup(func() { f.Close() })
	return f
}

// BenchmarkRawWriteAt: one WriteAt of 132 B. No engine at all.
func BenchmarkRawWriteAt(b *testing.B) {
	f := rawFile(b, 0)
	buf := make([]byte, rawRecordSize)
	b.SetBytes(rawRecordSize)
	b.ReportAllocs()
	b.ResetTimer()

	var off int64
	for i := 0; i < b.N; i++ {
		if _, err := f.WriteAt(buf, off); err != nil {
			b.Fatalf("WriteAt: %v", err)
		}
		off += rawRecordSize
	}
}

// BenchmarkRawReadAt: one ReadAt of 132 B from a warm file.
func BenchmarkRawReadAt(b *testing.B) {
	const records = benchReadCorpus
	f := rawFile(b, records*rawRecordSize)
	buf := make([]byte, rawRecordSize)
	b.SetBytes(rawRecordSize)
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		off := int64(i%records) * rawRecordSize
		if _, err := f.ReadAt(buf, off); err != nil {
			b.Fatalf("ReadAt: %v", err)
		}
	}
}

// BenchmarkRawReadAtTwice is the shape ReadRecord actually uses: read the fixed
// 20-byte header, learn the body length, then read the whole record. Two
// syscalls where one would do. Comparing this against BenchmarkRawReadAt prices
// the second syscall exactly, and comparing it against BenchmarkGet says how
// much of a Get is syscall and how much is engine.
func BenchmarkRawReadAtTwice(b *testing.B) {
	const records = benchReadCorpus
	f := rawFile(b, records*rawRecordSize)
	header := make([]byte, HeaderSize)
	buf := make([]byte, rawRecordSize)
	b.SetBytes(rawRecordSize)
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		off := int64(i%records) * rawRecordSize
		if _, err := f.ReadAt(header, off); err != nil {
			b.Fatalf("ReadAt header: %v", err)
		}
		if _, err := f.ReadAt(buf, off); err != nil {
			b.Fatalf("ReadAt body: %v", err)
		}
	}
}

// BenchmarkRawSync prices a bare fsync on this filesystem, with no write
// preceding it in the timed region beyond the one being flushed.
func BenchmarkRawSync(b *testing.B) {
	f := rawFile(b, 0)
	buf := make([]byte, rawRecordSize)
	b.ReportAllocs()
	b.ResetTimer()

	var off int64
	for i := 0; i < b.N; i++ {
		if _, err := f.WriteAt(buf, off); err != nil {
			b.Fatalf("WriteAt: %v", err)
		}
		if err := f.Sync(); err != nil {
			b.Fatalf("Sync: %v", err)
		}
		off += rawRecordSize
	}
}
