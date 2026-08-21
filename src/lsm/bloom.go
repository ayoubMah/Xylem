package lsm

import (
	"encoding/binary"
	"hash/fnv"
	"math"
)

// ---------------------------------------------------------------------------
// BLOOM FILTER
// ---------------------------------------------------------------------------
//
// The LSM's read path has one structural problem: a key can be in any SSTable,
// so a lookup for a key that DOES NOT EXIST is the worst case -- it must check
// every table and find nothing in each. That is the price paid for the cheap
// write path, and without help it grows linearly with the number of tables.
//
// A Bloom filter is the standard answer, and it is worth being exact about what
// it does, because it is a one-sided oracle:
//
//	mayContain(k) == false  ->  k is DEFINITELY NOT in this table. Certain.
//	mayContain(k) == true   ->  k is PROBABLY in this table. Might be wrong.
//
// So it can never cause a wrong answer, only wasted work. A false positive
// costs one unnecessary disk read; a false negative would be a correctness bug
// and the structure cannot produce one. That asymmetry is what makes it safe to
// put in front of the read path.
//
// HOW: a bit array of m bits and k hash functions. add(key) sets the k bits the
// hashes name. mayContain(key) checks those same k bits: if ANY is zero the key
// was never added (certain, because add would have set it); if all are one it
// may have been added, or k other keys may have collectively set those bits by
// coincidence.
//
// The parameters follow from the target false-positive rate p:
//
//	m/n = -log2(p) / ln(2)   bits per key
//	k   = (m/n) * ln(2)      hashes
//
// At p = 1%: ~9.6 bits per key and k = 7. That is the trade being made -- about
// ONE BYTE of RAM per key buys a 99% chance of skipping a disk read entirely.
// LevelDB ships 10 bits/key by default for the same reason.

const (
	// bitsPerKey targets a ~1% false-positive rate. Rounded up from 9.6.
	bitsPerKey = 10

	// maxBloomHashes caps k so a pathological size cannot make lookups slow.
	maxBloomHashes = 30
)

// bloomFilter is an immutable-after-write bit array.
type bloomFilter struct {
	bits []byte
	k    uint8 // number of hash functions
	m    uint32
}

// newBloom sizes a filter for n keys.
func newBloom(n int) *bloomFilter {
	if n < 1 {
		n = 1
	}

	m := uint32(n * bitsPerKey)
	// Round up to a whole byte so the encoding has no partial-byte edge case.
	if m%8 != 0 {
		m += 8 - m%8
	}

	k := uint8(math.Round(float64(bitsPerKey) * math.Ln2)) // = 7
	if k < 1 {
		k = 1
	}
	if k > maxBloomHashes {
		k = maxBloomHashes
	}

	return &bloomFilter{
		bits: make([]byte, m/8),
		k:    k,
		m:    m,
	}
}

// hashes derives the k bit positions for key.
//
// It does NOT run k independent hash functions. It runs ONE 64-bit hash and
// splits it into two 32-bit halves, then walks h1 + i*h2. This is the
// Kirsch-Mitzenmacher result: two hashes can simulate k of them with no
// measurable increase in the false-positive rate. It matters here because
// hashing is the entire cost of a filter probe, and doing it seven times would
// make the filter slower than the disk read it exists to avoid.
func (b *bloomFilter) hashes(key []byte) (h1, h2 uint32) {
	h := fnv.New64a()
	h.Write(key)
	sum := h.Sum64()
	return uint32(sum), uint32(sum >> 32)
}

func (b *bloomFilter) add(key []byte) {
	if b.m == 0 {
		return
	}
	h1, h2 := b.hashes(key)
	for i := uint8(0); i < b.k; i++ {
		pos := (h1 + uint32(i)*h2) % b.m
		b.bits[pos/8] |= 1 << (pos % 8)
	}
}

// mayContain reports whether key might be present. false is certain; true is
// probabilistic.
func (b *bloomFilter) mayContain(key []byte) bool {
	if b.m == 0 {
		// A filter with no bits cannot rule anything out, so it must say
		// "maybe" for everything. Answering false here would silently hide
		// every key in the table.
		return true
	}
	h1, h2 := b.hashes(key)
	for i := uint8(0); i < b.k; i++ {
		pos := (h1 + uint32(i)*h2) % b.m
		if b.bits[pos/8]&(1<<(pos%8)) == 0 {
			// One zero bit is proof of absence. Stop immediately -- the common
			// case for a miss is exiting on the first probe.
			return false
		}
	}
	return true
}

// encode serialises the filter: [k 1B][m 4B][bits...].
func (b *bloomFilter) encode() []byte {
	buf := make([]byte, 5+len(b.bits))
	buf[0] = b.k
	binary.BigEndian.PutUint32(buf[1:], b.m)
	copy(buf[5:], b.bits)
	return buf
}

// decodeBloom parses a filter. A zero-length buffer yields a filter that says
// "maybe" to everything, which is the correct behaviour for a table written
// before filters existed -- degraded performance, never a wrong answer.
func decodeBloom(buf []byte) (*bloomFilter, error) {
	if len(buf) == 0 {
		return &bloomFilter{m: 0}, nil
	}
	if len(buf) < 5 {
		return nil, ErrCorruptTable
	}
	k := buf[0]
	m := binary.BigEndian.Uint32(buf[1:])
	if k == 0 || k > maxBloomHashes || int(m/8) != len(buf)-5 {
		return nil, ErrCorruptTable
	}
	bits := make([]byte, len(buf)-5)
	copy(bits, buf[5:])
	return &bloomFilter{bits: bits, k: k, m: m}, nil
}
