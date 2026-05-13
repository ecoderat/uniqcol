// Package bench provides the benchmark harness and a deterministic
// synthetic row generator used to populate the report tables.
package bench

import (
	"math"
	"math/rand"

	"github.com/ecoderat/uniqcol/storage"
)

// BenchCountries is the fixed 20-element low-cardinality string set used
// by the country column. Kept small so RLE has something to bite into.
var BenchCountries = []string{
	"TR", "US", "DE", "GB", "FR", "ES", "IT", "NL", "BE", "SE",
	"FI", "NO", "DK", "PL", "AT", "CH", "PT", "IE", "GR", "JP",
}

// BenchSchema returns the standard 4-column schema used by every
// benchmark in this package, with event_id as the primary key.
func BenchSchema() storage.Schema {
	return storage.Schema{
		PK: "event_id",
		Columns: []storage.Column{
			{Name: "event_id", Type: storage.Int64},
			{Name: "user_id", Type: storage.Int64},
			{Name: "amount", Type: storage.Float64},
			{Name: "country", Type: storage.String},
		},
	}
}

// GenRows produces n rows that match BenchSchema. dupFraction in [0, 1]
// controls the fraction of rows whose event_id collides with an earlier
// row's; the duplicate count is exact, floor(dupFraction*n). seed makes
// generation reproducible.
//
// Uniques get monotonically increasing event_ids starting at 1; duplicates
// are drawn uniformly at random from the existing unique IDs and placed
// at the end of the slice. The output is NOT shuffled — callers that need
// interleaved order can shuffle with their own seed.
func GenRows(n int, dupFraction float64, seed int64) []storage.Row {
	if n <= 0 {
		return nil
	}
	if dupFraction < 0 {
		dupFraction = 0
	}
	if dupFraction > 1 {
		dupFraction = 1
	}
	r := rand.New(rand.NewSource(seed))

	// Need at least one unique to source duplicates from.
	dups := min(int(math.Floor(dupFraction*float64(n))), n-1)
	uniques := n - dups

	rows := make([]storage.Row, 0, n)
	for i := range uniques {
		rows = append(rows, storage.Row{
			int64(i + 1),
			int64(1 + r.Intn(100_000)),
			roundTo2(r.Float64() * 1000.0),
			BenchCountries[r.Intn(len(BenchCountries))],
		})
	}
	for range dups {
		src := rows[r.Intn(uniques)]
		// Copy: same PK as src, fresh non-PK fields, so the test "BF
		// caught a duplicate" exercises the BF path rather than full-row
		// equality.
		rows = append(rows, storage.Row{
			src[0],
			int64(1 + r.Intn(100_000)),
			roundTo2(r.Float64() * 1000.0),
			BenchCountries[r.Intn(len(BenchCountries))],
		})
	}
	return rows
}

func roundTo2(x float64) float64 {
	return math.Round(x*100) / 100
}

// EstimatedRowBytes is the rough per-row byte cost used by b.SetBytes in
// the write benchmarks. Sum of column widths: 8 + 8 + 8 + ~2 for the
// average country code.
const EstimatedRowBytes int64 = 8 + 8 + 8 + 2
