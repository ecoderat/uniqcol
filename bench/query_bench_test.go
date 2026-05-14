package bench

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ecoderat/uniqcol/query"
	"github.com/ecoderat/uniqcol/storage"
)

// BenchmarkQueryFilteredScan measures
//
//	SELECT amount WHERE country = 'TR'
//
// on a 1M-row segment where country is uniformly chosen from 20 codes
// (so ~5% expected selectivity). Includes parse + execute; segment open
// and segment build are outside the timer.
func BenchmarkQueryFilteredScan(b *testing.B) {
	const n = 1_000_000

	tbl, err := storage.CreateTable(BenchSchema(), storage.TableOptions{
		BloomDisabled: true, // we want exactly n rows in the segment
	})
	if err != nil {
		b.Fatalf("CreateTable: %v", err)
	}
	for _, r := range GenRows(n, 0, 1) {
		tbl.Insert(r)
	}
	path := filepath.Join(b.TempDir(), "scan.uniq")
	f, err := os.Create(path)
	if err != nil {
		b.Fatalf("create: %v", err)
	}
	if err := tbl.Flush(f); err != nil {
		_ = f.Close()
		b.Fatalf("flush: %v", err)
	}
	_ = f.Close()

	seg, err := storage.OpenSegment(path)
	if err != nil {
		b.Fatalf("OpenSegment: %v", err)
	}
	defer seg.Close()

	q, err := query.Parse("SELECT amount WHERE country = 'TR'")
	if err != nil {
		b.Fatalf("Parse: %v", err)
	}

	b.SetBytes(int64(n) * 8) // amount column = float64
	b.ResetTimer()

	start := time.Now()
	var hits int
	for range b.N {
		r, err := query.Execute(seg, q)
		if err != nil {
			b.Fatalf("Execute: %v", err)
		}
		hits = len(r.Rows)
	}
	elapsed := time.Since(start)
	rps := float64(n) * float64(b.N) / elapsed.Seconds()
	selectivity := float64(hits) / float64(n)
	b.ReportMetric(rps, "rows/sec")
	b.ReportMetric(selectivity*100, "selectivity_%")
	record("QueryFilteredScan/country=TR",
		fmt.Sprintf("n=%d iters=%d elapsed=%s rps=%.0f hits=%d selectivity=%.4f",
			n, b.N, elapsed.Round(time.Millisecond), rps, hits, selectivity))
}

// BenchmarkQueryGroupBy measures
//
//	SELECT country, COUNT(*) GROUP BY country
//
// on a 1M-row segment with country drawn uniformly from 20 codes (so
// 20 groups). Segment setup is outside the timer; parse + execute is
// inside. b.N>1 reuses the same parsed query and segment.
func BenchmarkQueryGroupBy(b *testing.B) {
	const n = 1_000_000

	tbl, err := storage.CreateTable(BenchSchema(), storage.TableOptions{
		BloomDisabled: true,
	})
	if err != nil {
		b.Fatalf("CreateTable: %v", err)
	}
	for _, r := range GenRows(n, 0, 1) {
		tbl.Insert(r)
	}
	path := filepath.Join(b.TempDir(), "groupby.uniq")
	f, err := os.Create(path)
	if err != nil {
		b.Fatalf("create: %v", err)
	}
	if err := tbl.Flush(f); err != nil {
		_ = f.Close()
		b.Fatalf("flush: %v", err)
	}
	_ = f.Close()

	seg, err := storage.OpenSegment(path)
	if err != nil {
		b.Fatalf("OpenSegment: %v", err)
	}
	defer seg.Close()

	q, err := query.Parse("SELECT country, COUNT(*) GROUP BY country")
	if err != nil {
		b.Fatalf("Parse: %v", err)
	}

	b.SetBytes(int64(n) * 2) // country values are 2-byte ASCII codes
	b.ResetTimer()

	start := time.Now()
	var groups int
	for range b.N {
		r, err := query.Execute(seg, q)
		if err != nil {
			b.Fatalf("Execute: %v", err)
		}
		groups = len(r.Rows)
	}
	elapsed := time.Since(start)
	rps := float64(n) * float64(b.N) / elapsed.Seconds()
	b.ReportMetric(rps, "rows/sec")
	b.ReportMetric(float64(groups), "groups")
	record("QueryGroupBy/country",
		fmt.Sprintf("n=%d iters=%d elapsed=%s rps=%.0f groups=%d",
			n, b.N, elapsed.Round(time.Millisecond), rps, groups))
}
