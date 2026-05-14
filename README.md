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
- [x] Disk üzerinde segment formatı (binary, kolon başına ayrı blok)
- [x] Run-Length Encoding (RLE) sıkıştırması
- [x] Segment okuma ve kolon-seçimli scan

### İterasyon 2 — Tekil Kayıt Katmanı
- [x] Bloom filter implementasyonu (parametrik FPR)
- [x] Yazma yoluna entegrasyon: `Insert(row)` → BF kontrolü → kabul/red
- [x] Birincil anahtar (primary key) konfigürasyonu
- [x] Yapılandırma: `expectedItems`, `targetFPR`

### İterasyon 3 — Sorgu ve Değerlendirme
- [x] CLI: `load <csv>`, `inspect <segment>`, `query "<sql>"`
- [x] `SELECT col1, col2 WHERE col = X` desteği
- [x] `COUNT`, `SUM` aggregation
- [x] Benchmark suite (`bench/`) + ham sonuç tabloları
- [x] Cyclomatic complexity ve test coverage raporları

> **Not:** Dictionary encoding, GROUP BY ve multi-segment merge bilinçli olarak bu prototipin dışında bırakılmıştır. Bkz. [Gelecek Çalışmalar](#gelecek-çalışmalar).

---

## Hızlı Başlangıç

### Kurulum

```bash
git clone https://github.com/ecoderat/uniqcol.git
cd uniqcol
go build -o uniqcol ./cmd/uniqcol
```

### Demo: CSV Yükle ve Segmenti İncele

```bash
# 1. Örnek CSV'yi yükle (yinelenen kayıtlar dahil)
./uniqcol load \
  --csv testdata/events.csv \
  --out data/events.uniq \
  --pk event_id \
  --schema event_id:int64,user_id:int64,amount:float64,country:string \
  --expected-items 1000000 \
  --target-fpr 0.01

# Beklenen çıktı (stdout):
#   rows read:        100,000
#   accepted:          97,843
#   rejected (BF):      2,157   (2.16% — probably duplicate)
#   parse errors:           0
#   wall time:          1.420s
#   throughput:        70,422 rows/sec
#   bloom est. FPR:    0.00940
#   segment size:        2.4 MB

# 2. Segment meta verisini incele (demo komutu)
./uniqcol inspect data/events.uniq

# Beklenen çıktı: format/şema/PK/Bloom parametreleri ve
# kolon başına payload boyutları (column pruning).

# 3. Sorgu çalıştır (FROM yok — segment --db ile verilir)
./uniqcol query --db data/events.uniq \
  "SELECT event_id, amount WHERE country = 'TR'"

# Çoklu koşul: AND, OR (AND, OR'dan önce bağlanır)
./uniqcol query --db data/events.uniq \
  "SELECT event_id, amount WHERE country = 'TR' AND amount > 50.0"

./uniqcol query --db data/events.uniq \
  "SELECT COUNT(*) WHERE country = 'TR' OR country = 'US'"

./uniqcol query --db data/events.uniq \
  "SELECT SUM(amount) WHERE amount > 50.0"

# CSV çıktısı (pipe için)
./uniqcol query --db data/events.uniq --format csv \
  "SELECT event_id, country" | head

# 4. Bloom filtresiz mod (benchmark/yazma-amaçlı)
./uniqcol load --no-bloom \
  --csv testdata/events.csv \
  --out data/events-nobf.uniq \
  --pk event_id \
  --schema event_id:int64,user_id:int64,amount:float64,country:string
```

### Programatik Kullanım (Go)

```go
import (
    "os"

    "github.com/ecoderat/uniqcol/query"
    "github.com/ecoderat/uniqcol/storage"
)

schema := storage.Schema{
    PK: "event_id",
    Columns: []storage.Column{
        {Name: "event_id", Type: storage.Int64},
        {Name: "user_id",  Type: storage.Int64},
        {Name: "amount",   Type: storage.Float64},
        {Name: "country",  Type: storage.String},
    },
}
tbl, _ := storage.CreateTable(schema, storage.TableOptions{
    BloomExpectedItems: 1_000_000,
    BloomTargetFPR:     0.01,
})

tbl.Insert(storage.Row{int64(1001), int64(42), 19.99, "TR"})
tbl.Insert(storage.Row{int64(1001), int64(42), 19.99, "TR"}) // reddedilir (BF)

f, _ := os.Create("events.uniq")
_ = tbl.Flush(f)
_ = f.Close()

seg, _ := storage.OpenSegment("events.uniq")
defer seg.Close()

q, _ := query.Parse("SELECT amount WHERE country = 'TR'")
result, _ := query.Execute(seg, q)
_ = result // result.Columns, result.Rows
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

Tekrar üretmek için: `go test -bench=. -benchmem -benchtime=1x ./bench/...`
(daha fazla ayrıntı: [`bench/README.md`](bench/README.md)).

Çalıştırma ortamı: `darwin/arm64`, Apple M4, Go 1.26. Tek koşumdan
alınan rakamlardır; üretim performans karşılaştırması değildir.

### Yazma Throughput'u (Bloom Filter ON vs OFF)

| Satır Sayısı | BF Kapalı (rows/sec) | BF Açık (rows/sec) | BF Kapalı Süre | BF Açık Süre | Overhead |
|---|---|---|---|---|---|
| 10K  | 11,097,240 | 3,179,439 | 1.0 ms   | 4.1 ms   | ~3.5× yavaşlama |
| 100K | 15,123,159 | 4,794,381 | 6.6 ms   | 20.9 ms  | ~3.2× yavaşlama |
| 1M   | 22,831,745 | 8,109,530 | 43.8 ms  | 123.4 ms | ~2.8× yavaşlama |

### Okuma (Filtreli Scan)

| Senaryo | Süre | Throughput |
|---|---|---|
| `SELECT amount WHERE country = 'TR'` (1M satır, ~%5 seçicilik) | 15.4 ms | 67.6 M rows/sec |
| `OpenSegment + ReadColumn("amount")` (1M satır) † | 10.9 ms | 91.6 M rows/sec |

† Filtresiz tam kolon taraması; `COUNT(*) WHERE` için alt sınır
proxy'sidir (gerçek filtreli sorgu yalnızca kabul edilen satırları
seçeceği için bunun kadar veya bundan hızlı olmalıdır).

### Sıkıştırma Oranı (RLE)

| Veri Profili (100K satır) | Ham Boyut | RLE Boyutu | Oran |
|---|---|---|---|
| Düşük kardinalite (20 country, round-robin) | 300,000 B | 400,000 B | **0.75** (kötü) |
| Yüksek kardinalite (rastgele int64)         | 800,000 B | 899,991 B | **0.89** (kötü) |
| Monotonik artan int64                       | 800,000 B | 900,000 B | **0.89** (kötü) |
| Kümeli int64 (100 run × 1000 tekrar)        | 800,000 B |   1,000 B | **800×** ✓ |

### Bloom Filter False Positive Oranı

100K kayıt için ölçüm + 1M kayıt için sınır hesabı
(`m = ceil(-n·ln p / (ln 2)²)` doğrusaldır, fark sadece ölçeklemedir).

| Hedef FPR | Ölçülen FPR (100K kayıt) | Bellek (100K kayıt) | Bellek (1M kayıt) |
|---|---|---|---|
| 1% (0.01)    | 1.242%   | 117 KB | ~1.14 MB |
| 0.1% (0.001) | 0.1014%  | 175 KB | ~1.71 MB |

### Yazılım Mühendisliği Metrikleri

| Metrik | Değer | Araç |
|---|---|---|
| Toplam LOC (test hariç) | 2,193 | `wc -l` |
| Test LOC                | 3,101 | `wc -l` |
| Cyclomatic complexity (max) | 28 (`storage.WriteSegment`, `storage.parseSegment`, `runLoad`) | `gocyclo` |
| Test coverage           | storage 90.3% / bloom 94.9% / cmd 90.8% / bench/datagen 100% | `go test -cover` |
| Paket sayısı            | 5 (`storage`, `bloom`, `cmd/uniqcol`, `bench`, kök) | — |

### Benchmark Sonuçları Üzerine Notlar

- **BF maliyeti ~3× yazma yavaşlaması.** Tek bir `Insert` hashing
  (FNV-1a 128, double hashing ile k=7 dizin türetme), k bitlik kontrol
  ve k bitlik kayıt anlamına geliyor. Allokasyon profili (BF açık: 1M
  alloc; BF kapalı: 175 alloc) maliyetin büyük kısmının PK byte
  dilimi başına `make([]byte, 8)` çağrısı olduğunu gösteriyor; bunlar
  bir sonraki iterasyonda `[]byte` stack tahsisleriyle veya kalıcı
  bir arabellekle düşürülebilir.
- **RLE rastgele/monoton int64'te kaybediyor.** Her satır kendi
  başına bir run (count=1) olarak kodlanıyor, bu da değer başına 1
  baytlık uvarint overhead'ı demek (8 → 9). 800.000 baytlık ham veri
  900.000 bayta çıkıyor. RLE yalnızca **ardışık tekrarları**
  sıkıştırır; sıralı olmak yetmez. README'nin "Gelecek Çalışmalar"
  bölümünde belirtilen dictionary encoding bu sınırlamayı çözmek
  için tasarlanmıştır. Kümeli profilde (1000 tekrarlı 100 değer) RLE
  beklendiği gibi davranıp 800× sıkıştırma sağlıyor.
- **Ölçülen FPR hedef değere oldukça yakın.** %1 hedefte ölçülen
  %1.24, %0.1 hedefte ölçülen %0.101 — Kirsch & Mitzenmacher double
  hashing yaklaşımının uniqcol'un kapasitesinde sapmadığını
  gösteriyor. Beklenen istatistiksel salınım çerçevesindedir.
- **Kolon okuma çok hızlı, çünkü mmap yok.** 91.6 M rows/sec rakamı
  segmentin `os.ReadFile` ile RAM'e tamamen yüklenmesinden sonra
  ölçülmektedir. Gerçek disk ilk-okuması çok daha yavaş olacaktır;
  bu özellikle mmap geçişi için bir TODO olarak `segment.go`'da
  belirtilmiştir.

---

## Gerçek Veri Üzerinde Doğrulama

Sentetik benchmark'ların yanında, motorun gerçek veride nasıl
davrandığını görmek için Kaggle'da yayınlanan bir e-ticaret veriseti
(541,909 satır, 25,900 unique `InvoiceNo`) üzerinde uçtan uca test
edildi.

### Bulgular

| Metrik             | Değer        | Yorum                                                  |
|--------------------|--------------|--------------------------------------------------------|
| Toplam satır       | 541,909      | Veriseti boyutu                                        |
| Unique `InvoiceNo` | 25,900       | Beklenen kabul edilecek kayıt sayısı                   |
| Kabul edilen       | 25,900       | Tam eşleşme — sıfır false negative                     |
| Reddedilen (BF)    | 516,009      | %95.22 — verisetinin gerçek duplicate oranı            |
| Yükleme süresi     | 149 ms       | ~3.6 M satır/sn throughput (uçtan uca, parse dahil)    |
| Segment boyutu     | 2.6 MB       | ~1.14 MB Bloom trailer + ~1.46 MB kolon payload        |
| `m` (BF bit sayısı)| 9,585,059    | `inspect` çıktısından doğrulandı                       |
| `k` (hash sayısı)  | 7            | Kirsch–Mitzenmacher (FNV-1a 128, ikili hash)           |
| Tahmini FPR        | 0.00000      | %1 hedef; kapasite altında olduğundan ~0               |

### Operasyonel Gözlem: Parametre Hassasiyeti

İlk denemede `--expected-items` parametresi "beklenen duplicate sayısı"
olarak yanlış yorumlandı ve `1,000,000` yerine küçük bir değer girildi.
Bunun sonucunda Bloom filter erken doyma noktasına ulaştı; sonraki
unique kayıtların büyük kısmı false positive ile reddedildi ve 25,900
unique kayıttan yalnızca **9,493**'ü hayatta kaldı (~%63 unique data
kaybı). Doğru parametre (`--expected-items 1000000`, `--target-fpr
0.01`) ile yeniden çalıştırıldığında tüm 25,900 unique kayıt eksiksiz
yakalandı.

Bu gözlem iki konuda somut girdi sağladı:

1. **CLI UX'i sertleştirildi.** `--expected-items` bayrağının açıklaması
   "beklenen duplicate sayısı / kabul edilecek satır sayısı" gibi yanlış
   yorumları engelleyecek şekilde yeniden yazıldı. Ayrıca iki uyarı
   eklendi: (a) `--expected-items < 1000` ise yükleme öncesinde, (b)
   tahmini FPR hedefin >2 katına çıkarsa yükleme sonrasında stderr'e
   tek satırlık uyarı basılır — yükleme kesilmez, ama tanı kullanıcıya
   açık edilir.
2. **"Gelecek Çalışmalar" listesindeki partitioned Bloom filter
   maddesinin gerekçesi güçlendi.** Segment başına bağımsız ve büyüme
   sırasında yeniden boyutlandırılabilen filterlar, tek-parametreli
   tasarımın bu hassasiyetini operasyonel olarak çözer.

### Kolon Bazlı Sıkıştırma Davranışı

Gerçek veri profili, sentetik benchmark tablolarındaki RLE bulgularıyla
nitel olarak örtüşmektedir. Kategorik / düşük kardinaliteli kolonlar
RLE'den faydalanır; tamamen unique veya yüksek-kardinaliteli string
kolonları yine değer başına 1 baytlık uvarint overhead'ı öder ve
sıkışmaz. Bu, dictionary encoding'in (bkz. *Gelecek Çalışmalar*) tipik
analitik veride sağlayacağı kazancın somut göstergesidir: özellikle
ürün açıklamaları gibi tekrar eden ama uzun string kolonları, dictionary
ile RLE'den çok daha verimli kodlanır.

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
- **Sorgu dili kısıtlıdır.** `query` alt komutu bilinçli olarak
  minimal tutulmuştur:
  - `SELECT <cols | * | COUNT(*) | SUM(col)>`
  - `[WHERE <cond> [(AND | OR) <cond>]* ]` — sınırsız sayıda koşul,
    `<cond>` = `<col> <op> <literal>`
  - Öncelik: `AND`, `OR`'dan önce bağlanır (standart SQL davranışı).
    Yani `a = 1 OR b = 2 AND c = 3` ifadesi `a = 1 OR (b = 2 AND c = 3)`
    olarak ayrıştırılır.
  - Operatörler: `= != < > <= >=`
  - Literal türleri: `int`, `float`, tek-tırnaklı `string`
  - `FROM` yoktur; segment `--db` ile verilir
  - **Desteklenmeyenler:** parantezler, `NOT`, iç içe ifadeler,
    kolon-kolona karşılaştırma (`WHERE a = b`), `IN` / `LIKE` /
    `BETWEEN`, `JOIN`, `GROUP BY`, `ORDER BY`, SQL içinde `LIMIT`
    (`--limit` CLI bayrağı yalnızca çıktı kesimi içindir).
  - Filtre literal tipi kolon tipiyle **tam eşleşmek zorundadır**;
    sessiz dönüşüm yapılmaz (`amount = 50` int64 olarak yazılır,
    `amount = 50.0` ise float64 olarak yorumlanır — kolon `float64`
    ise yalnızca ikincisi geçerlidir).
  - `SUM` sadece `int64` veya `float64` kolonlarda; int64 toplamında
    overflow algılanır ve hata döner.

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