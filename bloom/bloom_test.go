package bloom

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"testing"
)

func TestNewDerivesStandardParameters(t *testing.T) {
	f, err := New(1_000_000, 0.01)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Textbook values: m ≈ 9.585 M, k = 7.
	if f.M() < 9_500_000 || f.M() > 9_700_000 {
		t.Errorf("m = %d; expected ~9.58M", f.M())
	}
	if f.K() < 6 || f.K() > 8 {
		t.Errorf("k = %d; expected 7 (±1)", f.K())
	}
	// Bit array should be sized to ceil(m/8) bytes.
	wantBytes := (f.M() + 7) / 8
	if uint64(len(f.bits)) != wantBytes {
		t.Errorf("len(bits) = %d; want %d", len(f.bits), wantBytes)
	}
}

func TestNewClampsK(t *testing.T) {
	// Tiny n with extreme p drives the formula above maxK.
	f, err := New(2, 1e-20)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if f.K() != maxK {
		t.Errorf("k = %d; want clamp to %d", f.K(), maxK)
	}
}

func TestNewInvalidParameters(t *testing.T) {
	cases := []struct {
		name string
		n    uint64
		p    float64
	}{
		{"zero items", 0, 0.01},
		{"zero fpr", 100, 0},
		{"negative fpr", 100, -0.1},
		{"unity fpr", 100, 1.0},
		{"above unity fpr", 100, 1.5},
		{"nan fpr", 100, math.NaN()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(tc.n, tc.p); !errors.Is(err, ErrInvalidParameters) {
				t.Fatalf("err = %v; want ErrInvalidParameters", err)
			}
		})
	}
}

func TestNoFalseNegatives(t *testing.T) {
	f, err := New(10_000, 0.01)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	keys := make([][]byte, 10_000)
	for i := range keys {
		keys[i] = fmt.Appendf(nil, "key-%d", i)
		f.Add(keys[i])
	}
	for i, k := range keys {
		if !f.Contains(k) {
			t.Fatalf("false negative on key %d (%q)", i, k)
		}
	}
}

func TestMeasuredFPRWithinTarget(t *testing.T) {
	target := 0.01
	inserted := 10_000
	f, err := New(uint64(inserted), target)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for i := range inserted {
		f.Add(fmt.Appendf(nil, "in-%d", i))
	}
	queries := 100_000
	fp := 0
	for i := range queries {
		// Query namespace disjoint from inserted set.
		if f.Contains(fmt.Appendf(nil, "out-%d", i)) {
			fp++
		}
	}
	measured := float64(fp) / float64(queries)
	// 2x slack — statistical wiggle room; tighter bounds flake.
	if measured > 2*target {
		t.Errorf("measured FPR %.4f exceeds 2x target %.4f", measured, target)
	}
	t.Logf("measured FPR=%.4f target=%.4f estimated=%.4f m=%d k=%d",
		measured, target, f.EstimatedFPR(), f.M(), f.K())
}

func TestEstimatedFPRMonotonic(t *testing.T) {
	f, _ := New(1000, 0.01)
	if f.EstimatedFPR() != 0 {
		t.Errorf("estimated FPR before any Add = %g; want 0", f.EstimatedFPR())
	}
	prev := f.EstimatedFPR()
	for i := range 500 {
		f.Add(fmt.Appendf(nil, "k%d", i))
		cur := f.EstimatedFPR()
		if cur < prev {
			t.Fatalf("EstimatedFPR decreased after Add #%d: %g -> %g", i, prev, cur)
		}
		prev = cur
	}
}

func TestMarshalRoundTrip(t *testing.T) {
	f, _ := New(1000, 0.01)
	in := [][]byte{
		[]byte("hello"),
		[]byte("world"),
		[]byte(""),
		[]byte("\x00\x01\x02"),
		[]byte("héllo 世界 🚀"),
	}
	for _, k := range in {
		f.Add(k)
	}

	data, err := f.MarshalBinary()
	if err != nil {
		t.Fatalf("MarshalBinary: %v", err)
	}

	var g Filter
	if err := g.UnmarshalBinary(data); err != nil {
		t.Fatalf("UnmarshalBinary: %v", err)
	}
	if g.M() != f.M() || g.K() != f.K() || g.NumAdded() != f.NumAdded() {
		t.Fatalf("metadata mismatch: m=%d/%d k=%d/%d nAdded=%d/%d",
			g.M(), f.M(), g.K(), f.K(), g.NumAdded(), f.NumAdded())
	}
	for _, k := range in {
		if !g.Contains(k) {
			t.Fatalf("Contains(%q) returned false after round-trip", k)
		}
	}
	// Non-inserted keys: should match original filter's verdict.
	for _, k := range [][]byte{[]byte("nope"), []byte("xyz"), []byte("not-there")} {
		if g.Contains(k) != f.Contains(k) {
			t.Fatalf("Contains(%q) diverged across round-trip", k)
		}
	}
}

func TestUnmarshalErrors(t *testing.T) {
	f, _ := New(100, 0.01)
	f.Add([]byte("x"))
	valid, _ := f.MarshalBinary()

	cases := []struct {
		name    string
		mutate  func([]byte) []byte
		wantSub string
	}{
		{
			name:    "too short",
			mutate:  func(b []byte) []byte { return b[:5] },
			wantSub: "header too short",
		},
		{
			name:    "truncated bits",
			mutate:  func(b []byte) []byte { return b[:len(b)-1] },
			wantSub: "length mismatch",
		},
		{
			name: "k = 0",
			mutate: func(b []byte) []byte {
				c := bytes.Clone(b)
				c[8] = 0
				return c
			},
			wantSub: "k=0",
		},
		{
			name: "m = 0",
			mutate: func(b []byte) []byte {
				c := bytes.Clone(b)
				for i := range 8 {
					c[i] = 0
				}
				return c
			},
			wantSub: "m must be > 0",
		},
		{
			name: "bitsLen disagrees with m",
			mutate: func(b []byte) []byte {
				c := bytes.Clone(b)
				// Inflate m so derived (m+7)/8 no longer matches bitsLen.
				c[0] = 0xFF
				c[1] = 0xFF
				return c
			},
			wantSub: "does not match m",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var g Filter
			err := g.UnmarshalBinary(tc.mutate(valid))
			if !errors.Is(err, ErrCorruptFilter) {
				t.Fatalf("err = %v; want ErrCorruptFilter", err)
			}
		})
	}
}

func TestUnmarshalReusesFilter(t *testing.T) {
	// Calling UnmarshalBinary on an already-populated filter must replace
	// state cleanly; the test exercises the bits[:0] reuse path.
	f, _ := New(1000, 0.01)
	f.Add([]byte("first"))
	data, _ := f.MarshalBinary()

	g, _ := New(50_000, 0.05) // different size to force a slice-shape mismatch
	g.Add([]byte("decoy"))
	if err := g.UnmarshalBinary(data); err != nil {
		t.Fatalf("UnmarshalBinary: %v", err)
	}
	if g.M() != f.M() || g.K() != f.K() {
		t.Fatalf("re-init metadata mismatch")
	}
	if !g.Contains([]byte("first")) {
		t.Fatalf("Contains(\"first\") false after re-unmarshal")
	}
	if g.Contains([]byte("decoy")) {
		// The decoy was added to g before the unmarshal; after replacing
		// state it should be gone unless it happens to share bits.
		// FNV-1a("decoy") in a 1000-item filter is very unlikely to match.
		t.Logf("note: decoy still positive; rare but possible by chance")
	}
}
