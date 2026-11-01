# Artçı — Gerçek Zamanlı Deprem Veri Hattı

> **Bu bir eğitim projesidir; resmî bilgi için [AFAD](https://deprem.afad.gov.tr)'ı takip edin.**

AFAD, Kandilli Rasathanesi ve EMSC'den canlı deprem verisi toplayan, aynı
depremi üç kaynaktan gelen farklı değerlere rağmen **tek olaya** indiren,
haritada canlı gösteren, Telegram'dan abonelere **bir kez** haber veren ve
her olay için **doğrulanmış** kısa bir Türkçe özet yazan olay güdümlü bir veri hattı.

Go · PostgreSQL + PostGIS · Redis Streams · Prometheus + Grafana · Docker Compose + Caddy

- Canlı demo: _henüz kurulmadı_ (kurulum: [Canlıya alma](#canlıya-alma))
- Tasarım ve gerekçeler: [PLAN.md](PLAN.md) · [docs/MATCHING.md](docs/MATCHING.md) · [docs/NOTIFY.md](docs/NOTIFY.md)

## Problem

Aynı deprem dakikalar içinde üç kaynaktan gelir:

| Kaynak | Saat | Büyüklük | Konum |
|---|---|---|---|
| AFAD | 15:04:31 | 4,1 | 39,21 K · 28,15 D |
| Kandilli | 15:04:30 | 4,3 | 39,19 K · 28,17 D |
| EMSC | 15:04:32 | 4,2 | 39,23 K · 28,12 D |

Kullanıcı bunu **bir** deprem olarak görmeli, **bir** bildirim almalı. Artçı
dizisinde ise saniyeler arayla gerçekten birden çok deprem olur; bunlar
birleştirilmemeli.

## Sistem mimarisi

Artçı birbirinden bağımsız küçük Go servislerinden oluşur. Servisler birbirini
doğrudan çağırmaz; aralarındaki tek bağ iki Redis akışı ve ortak PostgreSQL
veritabanıdır. Böylece bir kaynak ya da servis çökünce diğerleri çalışmaya devam eder.

### Genel bakış

```mermaid
flowchart LR
    subgraph SRC["Dış kaynaklar"]
        AFADs[AFAD apiv2]
        KOERI[Kandilli lst0.asp]
        EMSCs[EMSC SeismicPortal]
    end

    subgraph IN["Toplayıcılar (cmd/ingest, kaynak başına ayrı süreç)"]
        A[ingest afad<br/>30 sn polling]
        K[ingest kandilli<br/>30 sn polling + windows-1254]
        E[ingest emsc<br/>WebSocket + FDSN boşluk doldurma]
    end

    AFADs --> A
    KOERI --> K
    EMSCs --> E

    A & K & E -->|"normalize.Report"| RS[(reports.raw)]
    A & K & E -. bozuk kayıt .-> DLQ[(reports.dlq)]

    RS -->|grup: merger| M[merger<br/>tekilleştirme]
    M <-->|tek transaction| PG[(PostgreSQL + PostGIS)]
    M -->|"yeni/değişen olay"| EV[(events.updated)]

    EV -->|grup: notifier| N[notifier]
    EV -->|grup: summarizer| S[summarizer]
    EV -->|"grup: api-&lt;host&gt;"| W[api]

    N <--> PG
    S <--> PG
    W <--> PG
    N --> TG[Telegram Bot API]
    S --> LLM[LLM sağlayıcısı<br/>isteğe bağlı]
    W -->|REST + WebSocket| MAP[Leaflet harita]
```

| Servis | Okur | Yazar | Görevi |
|---|---|---|---|
| `ingest -source=afad\|kandilli\|emsc` | kaynak | `reports.raw`, `reports.dlq` | Kaynağı ortak `Report` biçimine çevirir. Backoff + full jitter, kaynak başına circuit breaker; EMSC'de kopunca yeniden bağlanma ve kopma anından itibaren FDSN ile boşluk doldurma. Bozuk kayıt hattı durdurmaz, DLQ'ya gider. |
| `merger` | `reports.raw` | `reports`, `events`, `events.updated` | Raporu mevcut olaylarla eşleştirir; bağlar ya da yeni olay açar. Tek tüketici (bkz. [Neden tek merger?](#neden-tek-merger)). |
| `notifier` | `events.updated`, `subscriptions` | `notifications`, Telegram | Etkilenen aboneleri `ST_DWithin` ile bulur; Telegram hız sınırlarına uyarak gönderir. Aynı olay için ikinci bildirimi birincil anahtar engeller. |
| `summarizer` | `events.updated` | `summaries`, `llm_spend` | M ≥ 3,5 olaylar için özet yazar. Sayıları kod üretir, model yalnızca cümle kurar, doğrulayıcı her sayıyı denetler. |
| `api` | `events`, `events.updated` | WebSocket istemcileri | REST + WebSocket, gömülü Leaflet harita. |
| `replay` | katalog (JSONL), ISC bülteni | `reports.raw` | Sentetik ya da gerçek (ISC) bir deprem dizisini hızlandırarak oynatıp doğruluk ve gecikmeyi ölçer. |

### Bir raporun yolculuğu

Aynı depremin iki kaynaktan gelmesi:

```mermaid
sequenceDiagram
    autonumber
    participant K as ingest kandilli
    participant A as ingest afad
    participant R as Redis reports.raw
    participant M as merger
    participant DB as PostgreSQL
    participant E as Redis events.updated
    participant N as notifier / summarizer / api

    K->>R: XADD Report (kandilli, 15:04:30, M4,3)
    R->>M: XREADGROUP merger
    M->>DB: BEGIN · upsert reports · aday yok → events INSERT (v1) · COMMIT
    M->>E: XADD event v1
    M->>R: XACK
    E->>N: her grup kendi kopyasını alır
    N-->>N: bildirim gönderilir · harita güncellenir

    A->>R: XADD Report (afad, 15:04:31, M4,1)
    R->>M: XREADGROUP merger
    M->>DB: BEGIN · upsert · aday: v1 olayı, puan ≈ 0,90 → bağla<br/>gösterilen değerler AFAD'a geçer → v2 · COMMIT
    M->>E: XADD event v2
    M->>R: XACK
    E->>N: aynı olay, yeni sürüm
    N-->>N: büyüklük farkı 0,2 < 0,5 → ikinci bildirim yok · harita ve özet güncellenir
```

1. **Toplama.** Her toplayıcı kendi kaynağını okur, saat dilimini UTC'ye
   çevirir (Kandilli TRT, AFAD UTC ama eksiz), büyüklük türünü seçer ve
   `reports.raw` akışına yazar. Daha önce aynı hâliyle gördüğü kaydı yeniden yazmaz.
2. **Tekilleştirme.** `merger` raporu `(source, source_event_id)` ile upsert
   eder. Rapor yeniyse ±10 sn ve 50 km içindeki olaylar arasından en iyi
   puanlıyı arar; bağlar ya da yeni olay açar. Olayın gösterilen değerleri
   kaynak önceliğine göre yeniden hesaplanır, değiştiyse `version` artar.
3. **Yayın.** Değişen olay `events.updated` akışına yazılır. DB commit'i ile
   yayın arasında çökme olursa `events.published_version` eksik yayını
   sonradan tamamlar (küçük bir outbox).
4. **Tüketim.** Her tüketici kendi consumer group'uyla akışın tamamını alır:
   `notifier` bildirir, `summarizer` özet yazar, `api` haritadaki istemcilere iter.

### Akışlar

| Akış | Yazan | Tüketici grupları | Teslim |
|---|---|---|---|
| `reports.raw` | `ingest`, `replay` | `merger` | at-least-once; tekrar gelen mesaj upsert'te no-op olur |
| `reports.dlq` | `ingest` | — (inceleme için) | ayrıştırılamayan ham kayıt + neden |
| `events.updated` | `merger` | `notifier`, `summarizer`, `api-<host>` | at-least-once; tüketiciler `(event_id, version)` ile idempotent |

Akışlar `MAXLEN ≈ 100 000` ile sınırlıdır. Çöken bir tüketicinin onaylamadığı
mesajlar açılışta bekleyenler listesinden yeniden işlenir. `api` grubu `$`'dan
açılır: harita yalnızca canlı akışı ister, geçmişi REST'ten alır.

### Veri modeli

```mermaid
erDiagram
    events ||--o{ reports : "birleştirir"
    events ||--o{ notifications : ""
    events ||--o{ summaries : "sürüm başına"
    subscriptions ||--o{ notifications : ""

    events {
        uuid id PK
        timestamptz origin_time
        geography location "GIST"
        numeric magnitude
        int version "her değişiklikte +1"
        int published_version "outbox"
    }
    reports {
        bigserial id PK
        text source "UNIQUE(source, source_event_id)"
        text source_event_id
        timestamptz origin_time
        geography location
        jsonb raw "kaynağın orijinal kaydı"
        uuid event_id FK
    }
    subscriptions {
        bigserial id PK
        bigint chat_id
        geography center "il merkezi, kişisel konum değil"
        int radius_km
        numeric min_mag
    }
    notifications {
        uuid event_id PK
        bigint subscription_id PK
        text kind "initial | update"
    }
    summaries {
        uuid event_id PK
        int event_version PK
        text generator "llm:model | template"
        bool validated
    }
```

Her rapor `raw` alanında kaynağın orijinal kaydını saklar; tekilleştirme
kararı sonradan yeniden incelenebilir. `llm_spend` günlük LLM harcamasını tutar
(tavan dolunca şablona düşülür).

### Kurulum topolojisi

```mermaid
flowchart LR
    U[Kullanıcı] -->|443 TLS| C[Caddy<br/>Let's Encrypt]
    C --> API[api :8080]
    subgraph HOST["Tek sunucu, Docker Compose"]
        C
        API
        I1[ingest-afad] & I2[ingest-kandilli] & I3[ingest-emsc] & MG[merger] & NT[notifier] & SM[summarizer]
        PG[(postgres<br/>PostGIS 16)]
        RD[(redis 7<br/>AOF)]
        PR[Prometheus] --> GF[Grafana]
    end
    PR -. kazır /metrics .-> API & I1 & I2 & I3 & MG & NT & SM
    ADM[Yönetici] -->|SSH tüneli| GF
```

Tek bir imaj (`artci:latest`) tüm servisleri içerir; her servis Compose'da
farklı komutla başlar. Dışarıya yalnızca Caddy (80/443) açılır; Prometheus ve
Grafana yalnızca `127.0.0.1`'de dinler. Redis AOF ile kalıcıdır, böylece
yeniden başlatmada akışlardaki onaylanmamış mesajlar kaybolmaz.

## Tekilleştirme

Yeni rapor için (ayrıntı: [docs/MATCHING.md](docs/MATCHING.md)):

1. `(source, source_event_id)` zaten varsa bu bir **güncellemedir**; değişen bir şey yoksa dur (idempotent).
2. Zaman farkı ≤ **10 sn** ve mesafe ≤ **50 km** olan olaylar adaydır.
3. `puan = 1 − (|Δt|/10 sn × 0,4 + mesafe/50 km × 0,4 + |ΔM| × 0,2)`
4. En iyi puan ≥ **0,5** ve aday olayda **bu kaynaktan başka rapor yoksa** bağla; yoksa yeni olay.
5. Gösterilen değerler: AFAD > Kandilli > EMSC. Değiştiyse `version` +1, `events.updated`'a yaz.

### Eşikleri nasıl ayarladım

Plan'daki başlangıç eşikleri 60 sn / 100 km / 0,6 idi. Bir artçı dizisini
`replay` ile oynatınca, yoğun dönemde 60 sn'lik pencerede 3–10 farklı deprem
aday oluyordu: yanlış birleşen çiftlerin medyan zaman farkı 8 sn, doğru
çiftlerinki 0,9 sn idi. 217 eşik bileşimini iki gürültü düzeyinde taradım ve
yalnızca bir gürültü düzeyine iyi oturanı değil, ikisinde de sağlam olanı seçtim.

Sonra seçimi **gerçek** bir diziyle doğruladım: 10 Ağustos 2025 Sındırgı
(Mw 6,1) depreminin ilk 7 günü, [ISC bülteninden](http://www.isc.ac.uk/iscbulletin/)
AFAD ve Kandilli'nin kendi çözümleri (4201 deprem, 6949 rapor). ISC her
depremi tek olayda topladığı için hangi raporun hangi depreme ait olduğu
bağımsız olarak biliniyor.

| Eşikler | F1 sentetik | F1 sentetik (2× gürültü) | **F1 gerçek dizi** |
|---|---|---|---|
| Plan §8: 60 sn / 100 km / 0,6 | 0,847 | 0,851 | 0,944 |
| **Seçilen: 10 sn / 50 km / 0,5** | **0,959** | **0,954** | **0,986** |

Gerçek dizide seçilen eşikler 72 bileşimin en iyisinden yalnızca 0,0007
geride; değiştirmedim. Kalan hatanın çoğu doğru cevabın kendisinden: ISC'nin
otomatik gruplamasında 45 çift 10 sn / 50 km'den uzak, bu yüzden ulaşılabilir
recall ≈ 0,984 (ölçülen 0,981). Ayrıntı:
[docs/MATCHING.md → Gerçek dizide doğrulama](docs/MATCHING.md#gerçek-dizide-doğrulama).

> EMSC çözümleri ISC bülteninde yok; gerçek veri doğrulaması iki kaynaklıdır.

## Ölçüm sonuçları

**Gerçek dizi** (Sındırgı 2025, ilk 7 gün, ISC: AFAD + Kandilli; bellek içi merger, üç varış sırası):

| Ölçüm | Sonuç |
|---|---|
| Birleştirme precision / recall / F1 | **0,991 / 0,981 / 0,986** |
| Tam doğru birleştirilen deprem | %98,2 (4201 depremden) |

**Sentetik dizi** (gecikme ve iş hacmi ölçümleri için):

Sentetik Sındırgı dizisi (M6,1 + 24 saatte 400 artçı → 478 rapor, 262
gerçek deprem), yerel PostGIS + Redis, tek merger:

| Ölçüm | Sonuç |
|---|---|
| Birleştirme precision / recall / F1 | 0,899 / 0,947 / 0,922 |
| Tam doğru birleştirilen deprem | %88,9 |
| merger iş hacmi (bekletmeden oynatma, 5098 rapor) | ~220–350 rapor/sn |
| Olay gecikmesi, 10× hızda, dizinin en yoğun ilk 200 raporu (ilk rapor akışa → `events.updated`) | **p50 5 ms · p95 8 ms · p99 52 ms** |
| Aynı 200 rapor: precision / recall / F1 | 0,811 / 0,895 / 0,851 (en yoğun dönem; tüm dizide 0,922) |
| Olay gecikmesi, bekletmeden 478 rapor (kuyruk dolu) | p50 0,70 sn · p95 1,26 sn · p99 1,30 sn |
| 10 kat yoğun dizi (günde 4000 artçı) F1 | 0,70: açgözlü eşleştirmenin sınırı |

10× ölçümündeki tek uç değer (6,9 sn) hattın yavaşlığı değil. O olayın en
erken raporu 195,4. saniyede geldi, olay kimliği ise ilk kez 202,3. saniyede
(başka bir raporun geldiği anda) yayınlandı. Rapor önce başka bir olaya
bağlanıp sonra yeni olaya geçmiş görünüyor; ölçüm bunu gecikme olarak sayar.

Bu sayılar uçtan uca **bildirim** gecikmesi değildir: kaynakların kendi
yayın gecikmesi (dakikalar) ve Telegram gönderimi dahil değil. Plan'ın
"p95 < 10 sn" hedefi canlıda `artci_notify_latency_seconds` ile ölçülecek.

Yeniden üretmek için:

```sh
go run ./cmd/replay gen -out /tmp/sindirgi.jsonl
go run ./cmd/merger &                                   # yerel PostGIS + Redis gerekir
go run ./cmd/replay run -in /tmp/sindirgi.jsonl -speed 10 -cleanup
```

Gerçek dizi kataloğu repoda: [`testdata/replay/sindirgi-isc-2025-08.jsonl.gz`](testdata/replay/sindirgi-isc-2025-08.jsonl.gz)
(`go run ./cmd/replay isc` ile yeniden üretilir) ve her `go test`'te
oynatılır; doğruluk F1 0,98'in altına düşerse test kırılır.

Gerçek bir afetin verisini kullanırken bunu saygıyla ve yalnızca mühendislik
amacıyla yapıyorum. Kullanılan yalnızca kurumların yayımladığı deprem
parametreleridir (zaman, konum, büyüklük).

## Güvenilirlik

| Risk | Önlem |
|---|---|
| Kaynak geçici yanıt vermiyor | Exponential backoff + full jitter |
| Kaynak uzun süre çökük | Kaynak başına circuit breaker; diğerleri ayrı süreç, etkilenmez |
| Bozuk kayıt | `reports.dlq`; hat durmaz; aynı bozuk kayıt tekrar tekrar yazılmaz |
| Servis işi alıp çöker | Consumer group: onaylanmamış mesaj yeniden işlenir |
| Aynı mesaj tekrar gelir | `UNIQUE (source, source_event_id)` + upsert'in no-op'u |
| DB commit oldu, yayın olmadı | `events.published_version` (küçük bir outbox) |
| EMSC WebSocket kopar | Yeniden bağlanma + kopma anından itibaren FDSN ile boşluk doldurma; ping ile ölü bağlantı tespiti |
| Servis kapatılıyor | Graceful shutdown: yeni iş alma, eldekini bitir, ACK'le, çık |

### Neden exactly-once değil?

Redis Streams (ve aslında herhangi bir ağ) "tam olarak bir kez" teslim
garantisi veremez: tüketici işi yapıp ACK'lemeden önce çökerse mesaj yeniden
gelir. Bunun yerine **at-least-once teslim + idempotent işlem**: aynı rapor
ikinci kez gelirse `UNIQUE` kısıtı ve upsert onu etkisiz kılar; aynı bildirim
için `notifications` tablosunun birincil anahtarı ikinci gönderimi engeller.
Kullanıcının gördüğü etki "tam olarak bir kez"dir.

### Neden tek merger?

Eşleştirme "oku → karar ver → yaz" işlemidir. İki merger aynı depremin iki
raporunu aynı anda işlerse ikisi de aday bulamaz ve iki olay açar. Hacim
(sakin dönemde dakikada birkaç rapor, artçı fırtınasında saniyede birkaç)
tek tüketiciye fazlasıyla sığar: ölçülen iş hacmi saniyede ~220–350 rapor.
Ölçek gerekirse: bölgeye göre bölümleme ya da aday olayları kilitlemek.

### Neden Kafka değil?

Hacim düşük, ihtiyaç duyulan şey consumer group + onaylanmamış mesaj listesi
+ yeniden işleme. Redis Streams bunların hepsini tek, hafif bir süreçle verir.
Kafka'nın bölümleme, uzun saklama ve yüksek iş hacmi üstünlüğü burada
kullanılmazdı; işletme maliyeti ise kullanılırdı.

## Yapay zekâ özeti ve guardrail'ler

İlke: **model sayı üretmez, sadece cümle kurar.**

1. **Gerçekler (kod):** büyüklük `4,1`, saat `15:04` (Türkiye saati), derinlik, yer, en yakın il ve uzaklık (5 km'ye yuvarlı), kaynaklar.
2. **LLM:** bu nesneden 2–3 cümle, yalnızca `{"ozet": "..."}`.
3. **Doğrulayıcı (kod):** metindeki her sayı gerçeklerde birebir var mı; yazıyla sayı, tahmin ("daha büyüğü gelebilir"), panik ("felaket"), doğrulanamayan hasar ("binalar yıkıldı") ifadesi var mı; şema, uzunluk, biçim.
4. **Yedek:** LLM yoksa, hata verirse, reddedilirse ya da günlük harcama tavanı dolarsa şablonlu özet. Şablon 5000 rastgele olayda doğrulayıcıdan geçiyor.

Red-team seti: [`testdata/redteam/scenarios.json`](testdata/redteam/scenarios.json):
40 ret senaryosu (uydurma/yuvarlanmış sayı, UTC saat, yazıyla sayı, tahmin,
panik, hasar iddiası, prompt injection'a uyma, HTML, bağlantı, şema ihlali)
ve doğrulayıcının fazla katı olmadığını gösteren 8 kabul senaryosu. CI'da
her commit'te çalışır.

## Gözlem

Prometheus metrikleri ([`internal/metrics`](internal/metrics/metrics.go)),
Grafana panosu ([`deploy/grafana/dashboards/artci.json`](deploy/grafana/dashboards/artci.json)),
alarmlar ([`deploy/prometheus/alerts.yml`](deploy/prometheus/alerts.yml)):

- Bir kaynağa 15 dakikadır ulaşılamıyor _(Plan "rapor gelmiyor" diyordu; sakin saatlerde yanlış alarm vermesin diye erişime bakılıyor)_
- DLQ son 1 saatte 10'dan fazla kayıt aldı
- Bildirim gecikmesi p95 > 30 sn
- LLM özetlerinin %20'sinden fazlası reddediliyor

_Grafana ekran görüntüsü: canlı kurulumdan sonra eklenecek._

## Çalıştırma

### Yerel geliştirme

```sh
docker compose -f deploy/docker-compose.dev.yml up -d    # PostGIS + Redis
go run ./cmd/merger &
go run ./cmd/ingest --source=afad &
go run ./cmd/ingest --source=kandilli &
go run ./cmd/ingest --source=emsc &
go run ./cmd/summarizer &
go run ./cmd/notifier &          # ARTCI_TELEGRAM_TOKEN yoksa yalnızca loglar
go run ./cmd/api                 # http://localhost:8080
```

### Testler

```sh
go test ./...                                                   # birim
ARTCI_TEST_POSTGRES_DSN=postgres://artci:artci@localhost:5432/artci?sslmode=disable \
  go test ./...                                                 # + PostGIS entegrasyon
go test ./internal/summary -run RedTeam -v                      # red-team
ARTCI_SWEEP=1 go test ./internal/merger -run 'Sweep$' -v        # eşik taraması, sentetik
ARTCI_SWEEP=1 go test ./internal/merger -run SweepReal -v       # eşik taraması, gerçek dizi (~2 dk)
```

CI ([`.github/workflows/ci.yml`](.github/workflows/ci.yml)): gofmt, vet,
`go mod tidy` denetimi, red-team, PostGIS servisli `go test -race`, derleme,
Docker imajı.

### Canlıya alma

Tek sunucu, Docker Compose, Caddy ile otomatik TLS:

```sh
cp deploy/.env.example deploy/.env      # alan adı, parolalar, isteğe bağlı Telegram/LLM anahtarları
docker compose -f deploy/docker-compose.yml --env-file deploy/.env up -d --build
```

Dışarıya yalnızca 80/443 açılır. Grafana ve Prometheus yalnızca sunucuda
dinler: `ssh -L 3000:localhost:3000 sunucu` → http://localhost:3000.

## Veri kaynakları

| Kaynak | Erişim | Not |
|---|---|---|
| [AFAD](https://deprem.afad.gov.tr) | apiv2 olay listesi (JSON), 30 sn'de bir | Türkiye için resmî kaynak |
| [Kandilli Rasathanesi](http://www.koeri.boun.edu.tr) | "Son depremler" sayfası, 30 sn'de bir | Metin tablo; Windows-1254 olabilir |
| [EMSC](https://www.seismicportal.eu) | SeismicPortal WebSocket + FDSN | Dünya geneli; Türkiye ve çevresine filtrelenir |

İsteklerde iletişim bilgili açık bir `User-Agent` gönderilir
(`ARTCI_USER_AGENT`). Her olayda verinin hangi kurumlardan geldiği gösterilir.

Ayrıştırıcılar elle hazırlanmış girdilerin (bozuk kayıt senaryoları) yanında
31.10.2026'da kaynaklardan çekilmiş **gerçek** yanıtlarla da sınanır
([`testdata/source/real`](testdata/source/README.md)). Aynı depremin üç
kaynakta aynı UTC anına düştüğü test edilir: AFAD saatleri UTC ama eksiz,
Kandilli `lst0.asp` Türkiye saati (UTC+3).

## Etik ve sorumluluk

- **Resmî sistem gibi görünmez.** Sitede, botta ve burada: _"Bu bir eğitim projesidir; resmî bilgi için AFAD'ı takip edin."_
- **Tahmin yok.** Hiçbir bileşen "deprem olacak" ya da "daha büyüğü gelebilir" demez; doğrulayıcı bunu engeller.
- **Panik yok.** Dil sakin ve olgusal; emoji yalnızca büyüklük seviyesini gösterir.
- **Kişisel veri en azda.** Abonelikte kişinin konumu değil, seçtiği ilin merkezi saklanır; `/iptal` tüm kayıtları siler.
- **Kaynaklara saygı.** Makul sorgu aralıkları, değişmeyen kayıtları yeniden yazmama, açık `User-Agent`.

## Repo yapısı

```
cmd/          ingest · merger · notifier · summarizer · api · replay
internal/
  source/     kaynak istemcileri, ayrıştırıcılar, backoff, breaker, EMSC akışı
  normalize/  ortak Report / Event tipleri
  match/      eşleştirme (saf fonksiyonlar)
  merger/     akış + transaction, eşik taraması
  store/      PostgreSQL + PostGIS
  stream/     Redis Streams (at-least-once)
  notify/     Telegram botu ve bildirim kararları
  summary/    gerçekler, şablon, LLM, doğrulayıcı
  replay/     sentetik dizi, ISC'den gerçek dizi, doğruluk ve yüzdelik hesapları
  geo/ api/ metrics/ app/
migrations/   SQL göçleri (servislere gömülü)
testdata/     ayrıştırıcı girdileri (gerçek + elle), red-team, replay katalogları
web/          Leaflet harita (api'ye gömülü)
deploy/       compose (dev + canlı), Caddyfile, Prometheus, Grafana
```
