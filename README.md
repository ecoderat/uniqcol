# uniqcol

> Yazma anında tekil kayıt garantisi sunan, gömülebilir, kolon tabanlı minimal depolama motoru — Go ile.

`uniqcol`, kolon tabanlı (columnar) bir analitik depolama motorudur. Standart bir kolon tabanlı sistemden farklı olarak, yinelenen kayıtların temizlenmesini arka plan birleştirme (merge) işlemlerine bırakmaz; **yazma yoluna entegre edilmiş bir Bloom filter ile duplicate tespitini ingestion anında** gerçekleştirir. Proje, ClickHouse'un `ReplacingMergeTree` motorunda gözlemlenen "eventual deduplication" probleminin alternatif bir çözümünü minimal bir prototip üzerinde deneysel olarak değerlendirmeyi hedefler.

Bu repo, **Yazılım Mühendisliği yüksek lisans dersi proje ödevi** kapsamında geliştirilmektedir.

---

## İçindekiler

1. [Proje Durumu](#proje-durumu)
2. [Hızlı Başlangıç](#hızlı-başlangıç)
3. [Mimari](#mimari)
4. [Veri Formatı](#veri-formatı)
5. [Benchmark Sonuçları](#benchmark-sonuçları)
6. [Kapsam ve Sınırlamalar](#kapsam-ve-sınırlamalar)
7. [Gelecek Çalışmalar](#gelecek-çalışmalar)
8. [Referanslar](#referanslar)

---

## Proje Durumu

Proje **iteratif ve inkremental geliştirme modeli** ile yürütülmektedir. Sınırlı süre nedeniyle kapsam bilinçli olarak daraltılmıştır; ölçülebilir ve tutarlı sonuçlar üretmek, geniş ama yarım bir motor üretmekten önceliklidir.

### İterasyon 1 — Temel Kolon Tabanlı Depolama
- [x] Tablo şeması tanımı (kolon adı + tip)
- [x] Kolon bazlı bellek içi yazma tamponu (write buffer)
- [ ] Disk üzerinde segment formatı (binary, kolon başına ayrı blok)
- [ ] Run-Length Encoding (RLE) sıkıştırması
- [ ] Segment okuma ve kolon-seçimli scan

### İterasyon 2 — Tekil Kayıt Katmanı
- [ ] Bloom filter implementasyonu (parametrik FPR)
- [ ] Yazma yoluna entegrasyon: `Insert(row)` → BF kontrolü → kabul/red
- [ ] Birincil anahtar (primary key) konfigürasyonu
- [ ] Yapılandırma: `expectedItems`, `targetFPR`

### İterasyon 3 — Sorgu ve Değerlendirme
- [ ] CLI: `load <csv>`, `query <sql-benzeri>`
- [ ] `SELECT col1, col2 WHERE col = X` desteği
- [ ] `COUNT`, `SUM` aggregation
- [ ] Benchmark suite + sonuç grafikleri
- [ ] Cyclomatic complexity ve test coverage raporları

> **Not:** Dictionary encoding, GROUP BY ve multi-segment merge bilinçli olarak bu prototipin dışında bırakılmıştır. Bkz. [Gelecek Çalışmalar](#gelecek-çalışmalar).

---

## Hızlı Başlangıç

### Kurulum

```bash
git clone https://github.com/ecoderat/uniqcol.git
cd uniqcol
go build -o uniqcol ./cmd/uniqcol
```

### Demo: CSV Yükle ve Sorgu Çalıştır

```bash
# 1. Örnek CSV'yi yükle (yinelenen kayıtlar dahil)
./uniqcol load --csv testdata/events.csv --pk event_id --out data/events.uniq

# Beklenen çıktı:
#   yüklenen satır: 100000
#   kabul edilen:    97843
#   reddedilen (BF): 2157   (%2.16 yinelenen)
#   süre:           1.42s

# 2. Sorgu çalıştır
./uniqcol query --db data/events.uniq \
  "SELECT user_id, amount WHERE country = 'TR'"

# 3. Aggregation
./uniqcol query --db data/events.uniq \
  "SELECT COUNT(*), SUM(amount) WHERE country = 'TR'"
```

### Programatik Kullanım (Go)

```go
import "github.com/ecoderat/uniqcol"

table, _ := uniqcol.Create("events.uniq", uniqcol.Schema{
    PK: "event_id",
    Columns: []uniqcol.Column{
        {Name: "event_id", Type: uniqcol.Int64},
        {Name: "user_id",  Type: uniqcol.Int64},
        {Name: "amount",   Type: uniqcol.Float64},
        {Name: "country",  Type: uniqcol.String},
    },
    BloomFPR: 0.01,
})
defer table.Close()

table.Insert(uniqcol.Row{1001, 42, 19.99, "TR"})
table.Insert(uniqcol.Row{1001, 42, 19.99, "TR"}) // reddedilir

count := table.Query().Where("country", "==", "TR").Count()
```

---

## Mimari

### Yazma Yolu (Write Path)

```
Insert(row)
    │
    ▼
┌─────────────────────┐
│  Bloom Filter       │ ──── kayıt mevcut olabilir? ───► [hash kontrolü]
│  (in-memory)        │
└─────────────────────┘
    │ yeni kayıt (BF "not present")
    ▼
┌─────────────────────┐
│  Write Buffer       │ kolon başına slice'lar
│  (per-column)       │ append-only
└─────────────────────┘
    │ buffer dolduğunda flush
    ▼
┌─────────────────────┐
│  Disk Segment       │ kolon başına RLE-kodlu blok
│  (binary file)      │ + segment metadata
└─────────────────────┘
```

### Okuma Yolu (Read Path)

```
Query("SELECT a, b WHERE c = X")
    │
    ▼
┌─────────────────────┐
│  Column Pruning     │ sadece a, b, c kolonları okunur
└─────────────────────┘
    │
    ▼
┌─────────────────────┐
│  RLE Decode + Scan  │ filter (c = X) uygulanır
└─────────────────────┘
    │
    ▼
┌─────────────────────┐
│  Aggregator         │ COUNT / SUM (varsa)
└─────────────────────┘
```

### Paket Yapısı

```
uniqcol/
├── cmd/uniqcol/      # CLI giriş noktası
├── storage/          # Segment yazma/okuma, RLE
├── bloom/            # Bloom filter implementasyonu
├── query/            # Mini sorgu motoru
├── bench/            # Benchmark suite
├── testdata/         # Örnek CSV'ler
└── docs/             # Mimari notlar, deney sonuçları
```

---

## Veri Formatı

Her segment dosyası şu yapıdadır:

```
┌────────────────────────────────────────┐
│  HEADER                                │
│  - magic bytes (4)                     │
│  - version (2)                         │
│  - kolon sayısı (2)                    │
│  - satır sayısı (8)                    │
├────────────────────────────────────────┤
│  COLUMN 1 BLOCK                        │
│  - kolon adı, tip, encoding (RLE/raw)  │
│  - sıkıştırılmış payload               │
├────────────────────────────────────────┤
│  COLUMN 2 BLOCK                        │
│  ...                                   │
├────────────────────────────────────────┤
│  BLOOM FILTER                          │
│  - bit array + hash sayısı             │
└────────────────────────────────────────┘
```

---

## Benchmark Sonuçları

> **Durum:** Sonuçlar İterasyon 3 sonunda doldurulacaktır.

### Yazma Throughput'u (Bloom Filter ON vs OFF)

| Satır Sayısı | BF Kapalı | BF Açık | Overhead |
|---|---|---|---|
| 10K | TBD | TBD | TBD |
| 100K | TBD | TBD | TBD |
| 1M | TBD | TBD | TBD |

### Okuma (Filtreli Scan)

| Senaryo | Süre | Throughput |
|---|---|---|
| `SELECT col WHERE x = K` (1M satır, %1 seçicilik) | TBD | TBD |
| `COUNT(*) WHERE x = K` (1M satır) | TBD | TBD |

### Sıkıştırma Oranı (RLE)

| Veri Profili | Ham Boyut | Sıkıştırılmış | Oran |
|---|---|---|---|
| Düşük kardinalite (10 unique) | TBD | TBD | TBD |
| Yüksek kardinalite (rastgele) | TBD | TBD | TBD |

### Bloom Filter False Positive Oranı

| Hedef FPR | Ölçülen FPR | Bellek (1M kayıt için) |
|---|---|---|
| 1% | TBD | TBD |
| 0.1% | TBD | TBD |

### Yazılım Mühendisliği Metrikleri

| Metrik | Değer | Araç |
|---|---|---|
| Toplam LOC | TBD | `cloc` |
| Cyclomatic complexity (max) | TBD | `gocyclo` |
| Test coverage | TBD | `go test -cover` |
| Paket sayısı | 5 | — |

---

## Kapsam ve Sınırlamalar

Bu prototip **kavramsal bir doğrulama** amacı taşımaktadır; üretim kullanımı için tasarlanmamıştır. Bilinçli olarak dışarıda bırakılan özellikler:

- **Tek segment, tek tablo.** Multi-segment merge ve compaction yoktur.
- **Dictionary encoding yoktur.** Sadece RLE ve raw encoding desteklenir.
- **GROUP BY yoktur.** Sadece global aggregation (COUNT, SUM) çalışır.
- **Eşzamanlılık (concurrency) yoktur.** Tek yazıcı, tek okuyucu varsayımı geçerlidir.
- **Crash recovery / WAL yoktur.** Süreç düşerse buffer'daki veri kaybedilir.
- **Bloom filter sabit boyutludur.** Tablo başlangıcında konfigüre edilen kapasitenin üzerine çıkıldığında FPR artar; dinamik yeniden boyutlandırma yapılmaz.
- **Tip sistemi minimaldir.** `int64`, `float64`, `string`. Tarih/saat, decimal, nested tip desteği yoktur.

Bu sınırlamalar, sürenin makul tutulması ve **ölçülebilir, tutarlı sonuçlar** elde edilmesi adına bilinçli alınmış mühendislik kararlarıdır.

---

## Gelecek Çalışmalar

Bu prototipin doğal uzantıları:

1. **Dictionary encoding** — düşük kardinaliteli string kolonlar için ek sıkıştırma.
2. **Multi-segment + background merge** — ClickHouse benzeri bir parts modeli; segmentler arası dedup'un Bloom filter ile O(1) yapılması.
3. **Partitioned Bloom filter** — segment başına BF, tek büyük BF yerine. Bu, FPR'ın veri büyüdükçe sabit kalmasını sağlar.
4. **GROUP BY + hash aggregation** — tam analitik sorgu motoru.
5. **Crash recovery** — write-ahead log ekleme.
6. **Cuckoo filter karşılaştırması** — silme operasyonu destekleyen alternatif olasılıksal yapı.

---

## Referanslar

1. Stonebraker, M., et al. (2005). C-Store: A Column-oriented DBMS. *VLDB*.
2. Abadi, D. J., Boncz, P. A., & Harizopoulos, S. (2009). Column-oriented database systems. *VLDB Endowment*, 2(2).
3. Bloom, B. H. (1970). Space/time trade-offs in hash coding with allowable errors. *Communications of the ACM*, 13(7).
4. Raasveldt, M., & Mühleisen, H. (2019). DuckDB: an Embeddable Analytical Database. *SIGMOD*.
5. ClickHouse Documentation. (2025). ReplacingMergeTree Table Engine.

---

## Lisans

MIT (eğitim amaçlı, sorumluluk reddi geçerlidir).