package storage

import (
	"math"
	"reflect"
	"strings"
	"testing"
)

func TestRawRoundTripInt64(t *testing.T) {
	tests := []struct {
		name   string
		values []int64
	}{
		{"empty", []int64{}},
		{"single", []int64{42}},
		{"many", []int64{1, -1, 0, math.MaxInt64, math.MinInt64}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			enc := encodeRawInt64(tc.values)
			if len(enc) != 8*len(tc.values) {
				t.Fatalf("encoded len = %d; want %d", len(enc), 8*len(tc.values))
			}
			dec, err := decodeRawInt64(enc, len(tc.values))
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if len(tc.values) == 0 {
				if len(dec) != 0 {
					t.Fatalf("expected empty result, got %v", dec)
				}
				return
			}
			if !reflect.DeepEqual(dec, tc.values) {
				t.Fatalf("mismatch: got %v, want %v", dec, tc.values)
			}
		})
	}
}

func TestRawRoundTripFloat64(t *testing.T) {
	vals := []float64{1.5, -2.25, 0, math.Pi, math.Inf(1)}
	enc := encodeRawFloat64(vals)
	dec, err := decodeRawFloat64(enc, len(vals))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !reflect.DeepEqual(dec, vals) {
		t.Fatalf("mismatch: got %v, want %v", dec, vals)
	}
}

func TestRawRoundTripString(t *testing.T) {
	tests := []struct {
		name   string
		values []string
	}{
		{"empty", []string{}},
		{"with utf8 and empty", []string{"héllo", "", "世界", "🚀"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			enc := encodeRawString(tc.values)
			dec, err := decodeRawString(enc, len(tc.values))
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if len(tc.values) == 0 {
				if len(dec) != 0 {
					t.Fatalf("expected empty, got %v", dec)
				}
				return
			}
			if !reflect.DeepEqual(dec, tc.values) {
				t.Fatalf("mismatch: got %v, want %v", dec, tc.values)
			}
		})
	}
}

func TestRawDecodeErrors(t *testing.T) {
	if _, err := decodeRawInt64([]byte{1, 2, 3}, 1); err == nil {
		t.Fatalf("expected error on length mismatch")
	}
	if _, err := decodeRawFloat64([]byte{1, 2, 3}, 1); err == nil {
		t.Fatalf("expected error on length mismatch")
	}
	// Build a Raw-string blob with two strings but tell the decoder we expect 1.
	enc := encodeRawString([]string{"a", "b"})
	if _, err := decodeRawString(enc, 1); err == nil {
		t.Fatalf("expected error when blob has more strings than expectedRows")
	}
	// Tell the decoder we expect 5 from a 2-string blob.
	if _, err := decodeRawString(enc, 5); err == nil {
		t.Fatalf("expected error on too-few-strings")
	}
	// Bad length varint.
	bad := []byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}
	if _, err := decodeRawString(bad, 1); err == nil {
		t.Fatalf("expected error on bad varint")
	}
	// Truncated string body.
	tr := encodeRawString([]string{"abcdef"})
	if _, err := decodeRawString(tr[:len(tr)-2], 1); err == nil {
		t.Fatalf("expected error on truncated string body")
	}
}

func TestChooseEncoding(t *testing.T) {
	cases := []any{
		[]int64{1, 2, 3},
		[]float64{1.5},
		[]string{"a"},
		nil,
	}
	for _, c := range cases {
		if got := chooseEncoding(c); got != EncodingRLE {
			t.Errorf("chooseEncoding(%T) = %v; want EncodingRLE", c, got)
		}
	}
}

func TestEncodingString(t *testing.T) {
	cases := map[Encoding]string{
		EncodingRaw:  "raw",
		EncodingRLE:  "rle",
		Encoding(99): "Encoding(99)",
	}
	for e, want := range cases {
		if got := e.String(); got != want {
			t.Errorf("Encoding(%d).String() = %q; want %q", e, got, want)
		}
	}
	// Sanity: zero-value Encoding stringifies as "raw"
	var z Encoding
	if !strings.EqualFold(z.String(), "raw") {
		t.Errorf("zero Encoding.String() = %q; want raw", z.String())
	}
}
