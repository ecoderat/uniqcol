package bench

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/ecoderat/uniqcol/bloom"
	"github.com/ecoderat/uniqcol/storage"
)

// resultsSink appends one labeled line per benchmark to bench/results.txt.
// It is opened lazily and shared by every benchmark in this process.
var (
	sinkOnce sync.Once
	sinkFile *os.File
	sinkErr  error
)

func sink() *os.File {
	sinkOnce.Do(func() {
		// Open relative to the package directory, which `go test` uses as
		// the working dir.
		f, err := os.OpenFile("results.txt", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			sinkErr = err
			return
		}
		fmt.Fprintf(f, "\n=== run %s  GOOS=%s GOARCH=%s NumCPU=%d ===\n",
			time.Now().Format(time.RFC3339), runtime.GOOS, runtime.GOARCH, runtime.NumCPU())
		sinkFile = f
	})
	return sinkFile
}

func record(label, payload string) {
	if f := sink(); f != nil {
		fmt.Fprintf(f, "%s\t%s\n", label, payload)
	}
}

// ---------- BenchmarkWriteThroughput ----------
//
// Sub-benchmarks span N ∈ {10K, 100K, 1M} crossed with bloom={on, off}.
// Rows are generated once outside the timed region; the timed body
// re-inserts the same slice b.N times. This means b.N>1 simulates
// re-ingest cost rather than amortizing setup over more data — fine for
// our purposes, and -benchtime=1x makes it a single pass anyway.

func BenchmarkWriteThroughput(b *testing.B) {
	sizes := []int{10_000, 100_000, 1_000_000}
	for _, n := range sizes {
		for _, withBloom := range []bool{true, false} {
			label := "off"
			if withBloom {
				label = "on"
			}
			name := fmt.Sprintf("N=%d/bloom=%s", n, label)
			b.Run(name, func(b *testing.B) {
				rows := GenRows(n, 0, int64(n))
				schema := BenchSchema()
				opts := storage.TableOptions{
					BloomExpectedItems: uint64(n),
					BloomTargetFPR:     0.01,
					BloomDisabled:      !withBloom,
				}
				b.SetBytes(int64(n) * EstimatedRowBytes)
				b.ResetTimer()

				start := time.Now()
				for range b.N {
					tbl, err := storage.CreateTable(schema, opts)
					if err != nil {
						b.Fatalf("CreateTable: %v", err)
					}
					for _, r := range rows {
						tbl.Insert(r)
					}
				}
				elapsed := time.Since(start)

				// Per-iteration rate: total rows inserted / wall time.
				totalRows := float64(n) * float64(b.N)
				rps := totalRows / elapsed.Seconds()
				b.ReportMetric(rps, "rows/sec")
				record(name, fmt.Sprintf("rows=%d iters=%d elapsed=%s rps=%.0f",
					n, b.N, elapsed.Round(time.Millisecond), rps))
			})
		}
	}
}

// ---------- BenchmarkRLECompressionRatio ----------
//
// Not a true timing benchmark — measures encoded-size / raw-size for
// three representative column profiles. StopTimer at the top because we
// don't care about wall time and -benchmem would otherwise sample noise.

func BenchmarkRLECompressionRatio(b *testing.B) {
	const n = 100_000
	type profile struct {
		name string
		// build returns (rawBytes, rleBytes) for a single column of n
		// values shaped per the profile.
		build func() (rawBytes, rleBytes int)
	}
	profiles := []profile{
		{
			name: "low-cardinality-country",
			build: func() (int, int) {
				vals := make([]string, n)
				// Walk the country set in order, then repeat. RLE will
				// produce ~n/20 runs.
				for i := range vals {
					vals[i] = BenchCountries[i%len(BenchCountries)]
				}
				raw := 0
				for _, s := range vals {
					raw += len(s) + 1 // 1-byte uvarint for short strings
				}
				return raw, encodeColumnStringRLE(vals)
			},
		},
		{
			name: "high-cardinality-int64",
			build: func() (int, int) {
				rows := GenRows(n, 0, 1234)
				vals := make([]int64, n)
				for i, r := range rows {
					vals[i] = r[1].(int64) // user_id
				}
				return n * 8, encodeColumnInt64RLE(vals)
			},
		},
		{
			name: "sorted-monotonic-int64",
			build: func() (int, int) {
				// Monotonically increasing values: every consecutive pair
				// is distinct, so RLE produces n runs of count=1 and pays
				// the uvarint-count overhead per value. Included to make
				// the "RLE crushes sorted data" assumption testable;
				// spoiler: RLE only compresses CONSECUTIVE DUPLICATES,
				// not order.
				vals := make([]int64, n)
				for i := range vals {
					vals[i] = int64(i)
				}
				return n * 8, encodeColumnInt64RLE(vals)
			},
		},
		{
			name: "clustered-int64",
			build: func() (int, int) {
				// Long runs of the same value: 100 distinct values, each
				// repeated 1000 times in a row. This is where RLE shines
				// — the realistic shape of a sorted low-cardinality
				// column in an analytical workload.
				const runs = 100
				const runLen = n / runs
				vals := make([]int64, n)
				for r := range runs {
					for j := range runLen {
						vals[r*runLen+j] = int64(r)
					}
				}
				return n * 8, encodeColumnInt64RLE(vals)
			},
		},
	}
	for _, p := range profiles {
		b.Run(p.name, func(b *testing.B) {
			b.StopTimer()
			rawBytes, rleBytes := p.build()
			ratio := float64(rawBytes) / float64(rleBytes)
			b.ReportMetric(ratio, "ratio")
			b.ReportMetric(float64(rawBytes), "raw_bytes")
			b.ReportMetric(float64(rleBytes), "rle_bytes")
			record("RLE/"+p.name,
				fmt.Sprintf("raw=%d rle=%d ratio=%.3f", rawBytes, rleBytes, ratio))
		})
	}
}

// encodeColumnInt64RLE / encodeColumnStringRLE flush a single-column
// WriteBuffer through WriteSegment and return the resulting encoded
// payload length. We do this rather than calling the package-private
// encoders directly: it goes through the same code path the engine
// actually uses, and the storage package's chooseEncoding always picks
// RLE today.
func encodeColumnInt64RLE(vals []int64) int {
	schema := storage.Schema{
		PK:      "v",
		Columns: []storage.Column{{Name: "v", Type: storage.Int64}},
	}
	buf := storage.NewWriteBuffer(schema)
	for _, v := range vals {
		_ = buf.Append(storage.Row{v})
	}
	var b bytes.Buffer
	_ = storage.WriteSegment(&b, schema, buf, storage.WriteSegmentOpts{})
	return columnPayloadLen(&b, "v")
}

func encodeColumnStringRLE(vals []string) int {
	schema := storage.Schema{
		PK:      "v",
		Columns: []storage.Column{{Name: "v", Type: storage.String}},
	}
	buf := storage.NewWriteBuffer(schema)
	for _, v := range vals {
		_ = buf.Append(storage.Row{v})
	}
	var b bytes.Buffer
	_ = storage.WriteSegment(&b, schema, buf, storage.WriteSegmentOpts{})
	return columnPayloadLen(&b, "v")
}

// columnPayloadLen opens the encoded segment image and returns the
// PayloadLen of the named column. Avoids parsing wire bytes by hand.
func columnPayloadLen(b *bytes.Buffer, name string) int {
	tmp, err := os.CreateTemp("", "bench-col-*.uniq")
	if err != nil {
		panic(err)
	}
	tmpPath := tmp.Name()
	_, _ = io.Copy(tmp, b)
	_ = tmp.Close()
	defer os.Remove(tmpPath)

	seg, err := storage.OpenSegment(tmpPath)
	if err != nil {
		panic(err)
	}
	defer seg.Close()
	for _, ci := range seg.ColumnInfo() {
		if ci.Name == name {
			return ci.PayloadLen
		}
	}
	panic("column not found: " + name)
}

// ---------- BenchmarkBloomFPR ----------
//
// Insert n distinct keys, query 10n disjoint keys, report measured FPR
// alongside the target. Memory footprint of the bit array is also
// recorded so the README table can include "bellek (1M kayıt için)".

func BenchmarkBloomFPR(b *testing.B) {
	const n = 100_000
	for _, target := range []float64{0.01, 0.001} {
		name := fmt.Sprintf("target=%g", target)
		b.Run(name, func(b *testing.B) {
			b.StopTimer()
			f, err := bloom.New(uint64(n), target)
			if err != nil {
				b.Fatalf("bloom.New: %v", err)
			}
			var key [8]byte
			for i := range n {
				binary.LittleEndian.PutUint64(key[:], uint64(i))
				f.Add(key[:])
			}
			falsePositives := 0
			queries := 10 * n
			for i := range queries {
				binary.LittleEndian.PutUint64(key[:], uint64(n+i))
				if f.Contains(key[:]) {
					falsePositives++
				}
			}
			measured := float64(falsePositives) / float64(queries)
			bitsBytes := (f.M() + 7) / 8
			b.ReportMetric(measured, "measured_fpr")
			b.ReportMetric(target, "target_fpr")
			b.ReportMetric(float64(bitsBytes), "bytes")
			record("BloomFPR/"+name,
				fmt.Sprintf("n=%d target=%.4f measured=%.5f m=%d k=%d bytes=%d",
					n, target, measured, f.M(), f.K(), bitsBytes))
		})
	}
}

// ---------- BenchmarkSegmentReadColumn ----------
//
// Single-column scan latency: OpenSegment + ReadColumn(\"amount\") for a
// 1M-row segment. Setup (data generation, ingest, flush) is outside the
// timer; Close is too. b.N>1 just repeats the open+read.

func BenchmarkSegmentReadColumn(b *testing.B) {
	const n = 1_000_000
	dir := b.TempDir()
	path := filepath.Join(dir, "scan.uniq")

	// Setup table with BloomDisabled so we get exactly n rows into the
	// segment (a Bloom-enabled table would reject ~targetFPR*n unique
	// keys as false positives, leaving slightly fewer than n in the
	// flushed segment — which would skew the assertion below and isn't
	// what we're trying to measure here anyway).
	tbl, err := storage.CreateTable(BenchSchema(), storage.TableOptions{
		BloomDisabled: true,
	})
	if err != nil {
		b.Fatalf("CreateTable: %v", err)
	}
	rows := GenRows(n, 0, 7)
	for _, r := range rows {
		tbl.Insert(r)
	}
	f, err := os.Create(path)
	if err != nil {
		b.Fatalf("create: %v", err)
	}
	if err := tbl.Flush(f); err != nil {
		_ = f.Close()
		b.Fatalf("flush: %v", err)
	}
	_ = f.Close()

	b.SetBytes(int64(n) * 8) // amount is float64
	b.ResetTimer()

	start := time.Now()
	for range b.N {
		seg, err := storage.OpenSegment(path)
		if err != nil {
			b.Fatalf("OpenSegment: %v", err)
		}
		v, err := seg.ReadColumn("amount")
		if err != nil {
			b.Fatalf("ReadColumn: %v", err)
		}
		if got := len(v.([]float64)); got != n {
			b.Fatalf("len = %d; want %d", got, n)
		}
		seg.Close()
	}
	elapsed := time.Since(start)
	totalRows := float64(n) * float64(b.N)
	rps := totalRows / elapsed.Seconds()
	b.ReportMetric(rps, "rows/sec")
	record("ReadColumn/amount",
		fmt.Sprintf("n=%d iters=%d elapsed=%s rps=%.0f",
			n, b.N, elapsed.Round(time.Millisecond), rps))
}
