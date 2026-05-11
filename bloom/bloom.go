// Package bloom implements a parametric Bloom filter used by uniqcol's
// write-time deduplication path.
package bloom

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/fnv"
	"math"
)

// ErrInvalidParameters is returned by New when expectedItems is zero or
// targetFPR is outside (0, 1).
var ErrInvalidParameters = errors.New("bloom: invalid parameters")

// ErrCorruptFilter is returned by UnmarshalBinary on malformed input.
var ErrCorruptFilter = errors.New("bloom: corrupt filter")

const (
	// minK clamps the number of hash functions to at least 1.
	minK uint8 = 1
	// maxK clamps the number of hash functions. Beyond ~30 the marginal
	// reduction in FPR vanishes and per-op cost grows linearly.
	maxK uint8 = 30
)

// Filter is a classical Bloom filter parameterized at construction time.
// Not safe for concurrent use.
//
// Hash construction: the k hash positions are derived from a single
// 128-bit FNV-1a sum split into two 64-bit halves h1, h2 (high and low),
// then combined with the double-hashing scheme of Kirsch & Mitzenmacher
// (2006, "Less Hashing, Same Performance"): h_i(x) = (h1 + i*h2) mod m.
//
// The original design called for hash/maphash plus hash/fnv as the two
// streams; that was changed because hash/maphash seeds are opaque and
// cannot be serialized — Marshal/Unmarshal across a process boundary
// would silently change which keys hash to which bits. FNV-1a 128 split
// is deterministic by construction and gives two independent 64-bit
// streams from a single well-vetted algorithm.
type Filter struct {
	m      uint64
	k      uint8
	bits   []byte
	nAdded uint64
}

// New constructs a Filter sized to hold expectedItems with the requested
// false-positive rate. The optimal sizing follows the standard formulas:
//
//	m = ceil(-n * ln(p) / (ln 2)^2)
//	k = round((m / n) * ln 2)   clamped to [1, 30]
//
// Returns ErrInvalidParameters if expectedItems == 0 or targetFPR is not
// in (0, 1).
func New(expectedItems uint64, targetFPR float64) (*Filter, error) {
	if expectedItems == 0 {
		return nil, fmt.Errorf("%w: expectedItems must be > 0", ErrInvalidParameters)
	}
	if !(targetFPR > 0) || !(targetFPR < 1) {
		return nil, fmt.Errorf("%w: targetFPR must be in (0, 1), got %g", ErrInvalidParameters, targetFPR)
	}
	n := float64(expectedItems)
	ln2 := math.Ln2
	mFloat := -n * math.Log(targetFPR) / (ln2 * ln2)
	if mFloat <= 0 || mFloat > float64(math.MaxInt64) {
		return nil, fmt.Errorf("%w: derived m=%g out of range", ErrInvalidParameters, mFloat)
	}
	m := uint64(math.Ceil(mFloat))
	if m == 0 {
		m = 1
	}
	kFloat := math.Round(float64(m) / n * ln2)
	var k uint8
	switch {
	case kFloat < float64(minK):
		k = minK
	case kFloat > float64(maxK):
		k = maxK
	default:
		k = uint8(kFloat)
	}
	return &Filter{
		m:    m,
		k:    k,
		bits: make([]byte, (m+7)/8),
	}, nil
}

// Add inserts key into the filter. Duplicate Adds are idempotent on the
// bit array but each still increments NumAdded (the FPR formula
// approximates saturation by call count, so callers that only Add
// genuinely new keys — as Table.Insert does — get an accurate estimate).
func (f *Filter) Add(key []byte) {
	h1, h2 := hash128(key)
	for i := range f.k {
		idx := (h1 + uint64(i)*h2) % f.m
		f.bits[idx/8] |= 1 << (idx % 8)
	}
	f.nAdded++
}

// Contains reports whether key may be in the set. False positives are
// possible at approximately the rate returned by EstimatedFPR; false
// negatives are not.
func (f *Filter) Contains(key []byte) bool {
	h1, h2 := hash128(key)
	for i := range f.k {
		idx := (h1 + uint64(i)*h2) % f.m
		if f.bits[idx/8]&(1<<(idx%8)) == 0 {
			return false
		}
	}
	return true
}

// EstimatedFPR returns the expected false-positive rate given the
// number of items added so far:
//
//	(1 - exp(-k*n/m))^k
//
// Returns 0 when no items have been added.
func (f *Filter) EstimatedFPR() float64 {
	if f.nAdded == 0 {
		return 0
	}
	inner := 1 - math.Exp(-float64(f.k)*float64(f.nAdded)/float64(f.m))
	return math.Pow(inner, float64(f.k))
}

// M returns the bit array size in bits.
func (f *Filter) M() uint64 { return f.m }

// K returns the number of hash functions.
func (f *Filter) K() uint8 { return f.k }

// NumAdded returns the number of Add calls made on this filter.
func (f *Filter) NumAdded() uint64 { return f.nAdded }

// MarshalBinary returns the filter body, without magic / version
// envelope; the segment layer prepends those on write and strips them
// on read.
//
// Format:
//
//	[m: uint64 LE][k: uint8][nAdded: uint64 LE][bitsLen: uvarint][bits]
//
// nAdded is persisted so EstimatedFPR remains meaningful after reload.
func (f *Filter) MarshalBinary() ([]byte, error) {
	var ub [binary.MaxVarintLen64]byte
	out := make([]byte, 0, 8+1+8+binary.MaxVarintLen64+len(f.bits))
	var u8 [8]byte
	binary.LittleEndian.PutUint64(u8[:], f.m)
	out = append(out, u8[:]...)
	out = append(out, f.k)
	binary.LittleEndian.PutUint64(u8[:], f.nAdded)
	out = append(out, u8[:]...)
	n := binary.PutUvarint(ub[:], uint64(len(f.bits)))
	out = append(out, ub[:n]...)
	out = append(out, f.bits...)
	return out, nil
}

// UnmarshalBinary populates f from MarshalBinary output. Returns
// ErrCorruptFilter on malformed input.
func (f *Filter) UnmarshalBinary(data []byte) error {
	const fixedPrefix = 8 + 1 + 8
	if len(data) < fixedPrefix {
		return fmt.Errorf("%w: header too short (%d bytes)", ErrCorruptFilter, len(data))
	}
	m := binary.LittleEndian.Uint64(data[0:8])
	k := data[8]
	nAdded := binary.LittleEndian.Uint64(data[9:17])
	rest := data[fixedPrefix:]
	bitsLen, n := binary.Uvarint(rest)
	if n <= 0 {
		return fmt.Errorf("%w: bad bits length", ErrCorruptFilter)
	}
	rest = rest[n:]
	if uint64(len(rest)) != bitsLen {
		return fmt.Errorf("%w: bits payload length mismatch (have %d, want %d)", ErrCorruptFilter, len(rest), bitsLen)
	}
	if m == 0 {
		return fmt.Errorf("%w: m must be > 0", ErrCorruptFilter)
	}
	expectedBitsLen := (m + 7) / 8
	if bitsLen != expectedBitsLen {
		return fmt.Errorf("%w: bits length %d does not match m=%d (want %d)", ErrCorruptFilter, bitsLen, m, expectedBitsLen)
	}
	if k < minK || k > maxK {
		return fmt.Errorf("%w: k=%d out of range [%d, %d]", ErrCorruptFilter, k, minK, maxK)
	}
	f.m = m
	f.k = k
	f.nAdded = nAdded
	f.bits = append(f.bits[:0], rest...)
	return nil
}

// hash128 returns two 64-bit hashes by splitting the 128-bit FNV-1a sum
// of key into a high and low half.
func hash128(key []byte) (uint64, uint64) {
	h := fnv.New128a()
	_, _ = h.Write(key)
	sum := h.Sum(nil)
	return binary.BigEndian.Uint64(sum[0:8]), binary.BigEndian.Uint64(sum[8:16])
}
