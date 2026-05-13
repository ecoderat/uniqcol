package main

import (
	"fmt"
	"strconv"
)

// commaSepInt64 returns n formatted with thousand-separator commas,
// e.g. 1234567 -> "1,234,567".
func commaSepInt64(n int64) string {
	if n < 0 {
		return "-" + commaSepUint64(uint64(-n))
	}
	return commaSepUint64(uint64(n))
}

// commaSepUint64 returns n formatted with thousand-separator commas.
func commaSepUint64(n uint64) string {
	s := strconv.FormatUint(n, 10)
	if len(s) <= 3 {
		return s
	}
	// Walk from the right, inserting a comma every three digits.
	out := make([]byte, 0, len(s)+(len(s)-1)/3)
	first := len(s) % 3
	if first == 0 {
		first = 3
	}
	out = append(out, s[:first]...)
	for i := first; i < len(s); i += 3 {
		out = append(out, ',')
		out = append(out, s[i:i+3]...)
	}
	return string(out)
}

// humanBytes returns n formatted in a human-friendly unit (B, KB, MB,
// GB, TB). Uses base 1024 because that's what's most legible for
// segment files where every payload is a power-of-two-ish slice.
func humanBytes(n int64) string {
	const (
		KB = 1024
		MB = KB * 1024
		GB = MB * 1024
		TB = GB * 1024
	)
	switch {
	case n < KB:
		return fmt.Sprintf("%d B", n)
	case n < MB:
		return fmt.Sprintf("%.1f KB", float64(n)/KB)
	case n < GB:
		return fmt.Sprintf("%.1f MB", float64(n)/MB)
	case n < TB:
		return fmt.Sprintf("%.1f GB", float64(n)/GB)
	default:
		return fmt.Sprintf("%.1f TB", float64(n)/TB)
	}
}
