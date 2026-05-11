package storage

import (
	"errors"
	"math"
	"reflect"
	"testing"
)

func TestRLERoundTripInt64(t *testing.T) {
	tests := []struct {
		name   string
		values []int64
	}{
		{"empty", []int64{}},
		{"single", []int64{42}},
		{"all same", []int64{7, 7, 7, 7, 7}},
		{"all distinct", []int64{1, 2, 3, 4, 5}},
		{"mixed runs", []int64{1, 1, 1, 2, 2, 3, 4, 4, 4, 4}},
		{"extremes", []int64{math.MinInt64, math.MinInt64, 0, math.MaxInt64}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			enc := encodeRLEInt64(tc.values)
			dec, err := decodeRLEInt64(enc, len(tc.values))
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
				t.Fatalf("round-trip mismatch:\n got = %v\nwant = %v", dec, tc.values)
			}
		})
	}
}

func TestRLERoundTripFloat64(t *testing.T) {
	tests := []struct {
		name   string
		values []float64
	}{
		{"empty", []float64{}},
		{"single", []float64{3.14}},
		{"all same", []float64{2.5, 2.5, 2.5}},
		{"all distinct", []float64{1.0, 1.5, 2.0, 2.5}},
		{"mixed runs", []float64{1.0, 1.0, 2.0, 3.0, 3.0, 3.0}},
		{"negatives and zero", []float64{-1.5, -1.5, 0, 0, math.Pi}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			enc := encodeRLEFloat64(tc.values)
			dec, err := decodeRLEFloat64(enc, len(tc.values))
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
				t.Fatalf("round-trip mismatch:\n got = %v\nwant = %v", dec, tc.values)
			}
		})
	}
}

func TestRLERoundTripString(t *testing.T) {
	tests := []struct {
		name   string
		values []string
	}{
		{"empty slice", []string{}},
		{"single", []string{"hello"}},
		{"all same", []string{"x", "x", "x", "x"}},
		{"all distinct", []string{"a", "b", "c"}},
		{"mixed runs", []string{"TR", "TR", "TR", "US", "DE", "DE"}},
		{"utf8 multibyte", []string{"héllo", "héllo", "世界", "🚀", "🚀"}},
		{"empty strings", []string{"", "", "x", "", ""}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			enc := encodeRLEString(tc.values)
			dec, err := decodeRLEString(enc, len(tc.values))
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
				t.Fatalf("round-trip mismatch:\n got = %v\nwant = %v", dec, tc.values)
			}
		})
	}
}

func TestRLEDecodeErrorsInt64(t *testing.T) {
	enc := encodeRLEInt64([]int64{1, 2, 3})

	t.Run("truncated value", func(t *testing.T) {
		if _, err := decodeRLEInt64(enc[:len(enc)-1], 3); !errors.Is(err, ErrRLECorrupt) {
			t.Fatalf("err = %v; want ErrRLECorrupt", err)
		}
	})
	t.Run("expectedRows too low", func(t *testing.T) {
		if _, err := decodeRLEInt64(enc, 2); !errors.Is(err, ErrRLECorrupt) {
			t.Fatalf("err = %v; want ErrRLECorrupt", err)
		}
	})
	t.Run("expectedRows too high", func(t *testing.T) {
		if _, err := decodeRLEInt64(enc, 10); !errors.Is(err, ErrRLECorrupt) {
			t.Fatalf("err = %v; want ErrRLECorrupt", err)
		}
	})
	t.Run("bad varint", func(t *testing.T) {
		// 10 consecutive 0xFF bytes overflow uvarint
		bad := []byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}
		if _, err := decodeRLEInt64(bad, 1); !errors.Is(err, ErrRLECorrupt) {
			t.Fatalf("err = %v; want ErrRLECorrupt", err)
		}
	})
}

func TestRLEDecodeErrorsFloat64(t *testing.T) {
	enc := encodeRLEFloat64([]float64{1.0, 2.0})
	if _, err := decodeRLEFloat64(enc[:len(enc)-2], 2); !errors.Is(err, ErrRLECorrupt) {
		t.Fatalf("err = %v; want ErrRLECorrupt", err)
	}
	if _, err := decodeRLEFloat64(enc, 1); !errors.Is(err, ErrRLECorrupt) {
		t.Fatalf("err = %v; want ErrRLECorrupt", err)
	}
	bad := []byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}
	if _, err := decodeRLEFloat64(bad, 1); !errors.Is(err, ErrRLECorrupt) {
		t.Fatalf("err = %v; want ErrRLECorrupt", err)
	}
}

func TestRLEDecodeErrorsString(t *testing.T) {
	enc := encodeRLEString([]string{"hello", "world"})

	t.Run("truncated body", func(t *testing.T) {
		if _, err := decodeRLEString(enc[:len(enc)-1], 2); !errors.Is(err, ErrRLECorrupt) {
			t.Fatalf("err = %v; want ErrRLECorrupt", err)
		}
	})
	t.Run("expectedRows mismatch", func(t *testing.T) {
		if _, err := decodeRLEString(enc, 5); !errors.Is(err, ErrRLECorrupt) {
			t.Fatalf("err = %v; want ErrRLECorrupt", err)
		}
	})
	t.Run("bad run count varint", func(t *testing.T) {
		bad := []byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}
		if _, err := decodeRLEString(bad, 1); !errors.Is(err, ErrRLECorrupt) {
			t.Fatalf("err = %v; want ErrRLECorrupt", err)
		}
	})
	t.Run("bad string length varint", func(t *testing.T) {
		// valid count=1 followed by 10 0xFF bytes (overflow)
		bad := []byte{0x01, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}
		if _, err := decodeRLEString(bad, 1); !errors.Is(err, ErrRLECorrupt) {
			t.Fatalf("err = %v; want ErrRLECorrupt", err)
		}
	})
}
