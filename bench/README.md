# uniqcol benchmarks

Run the full suite (single run per sub-benchmark):

```bash
go test -bench=. -benchmem -benchtime=1x ./bench/...
```

`-benchtime=1x` is intentional. The default 1-second target makes Go run
each `b.N` value up the ladder (1, 10, 100, ...) until elapsed > 1 s and
then divide — meaningless for "how long does a 1M-row insert actually
take." One pass is the right call for throughput / ratio measurements.

For statistical confidence, re-run several times and average. Standard
benchmark output is also appended to `results.txt` (one labeled line per
sub-benchmark) for ingest into the report tables.

## What each benchmark measures

| Name                          | What it answers                                                |
|-------------------------------|----------------------------------------------------------------|
| `BenchmarkWriteThroughput`    | rows/sec at 10K / 100K / 1M, with Bloom on vs off              |
| `BenchmarkRLECompressionRatio`| raw bytes / RLE bytes for four column shapes                   |
| `BenchmarkBloomFPR`           | measured FPR vs target on 100K inserts + 1M disjoint queries   |
| `BenchmarkSegmentReadColumn`  | OpenSegment + ReadColumn("amount") latency on a 1M-row segment |

Setup work (row generation, segment build) is outside the timed region
in every case. Flushing to disk is intentionally NOT included in
`BenchmarkWriteThroughput` because disk I/O dominates the signal we
want to measure (the cost of `Insert`, which is where the Bloom filter
lives).
