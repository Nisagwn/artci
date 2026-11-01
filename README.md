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

## Mimari

```mermaid
flowchart LR
    subgraph IN["Toplayıcılar (cmd/ingest)"]
        E[emsc<br/>WebSocket + FDSN]
        A[afad<br/>polling]
        K[kandilli<br/>polling + parse]
    end

    E & A & K -->|normalize| RS[(Redis Streams<br/>reports.raw)]
    E & A & K -. bozuk kayıt .-> DLQ[(reports.dlq)]

    RS --> M[merger<br/>tekilleştirme]
    M --> PG[(PostgreSQL + PostGIS)]
    M --> EV[(Redis Streams<br/>events.updated)]

    EV --> N[notifier<br/>Telegram]
    EV --> S[summarizer<br/>gerçekler + LLM + doğrulayıcı]
    EV --> W[api<br/>REST + WebSocket]
    S --> PG
    N --> PG
    W --> MAP[Leaflet harita]

    subgraph OBS["Gözlem"]
        P[Prometheus] --> G[Grafana]
    end
```

| Servis | Görevi |
|---|---|
| `ingest --source=afad\|kandilli\|emsc` | Kaynağı okur, ortak `Report` biçimine çevirir, `reports.raw`'a yazar. Backoff + jitter, kaynak başına circuit breaker; EMSC'de kopunca yeniden bağlanma ve FDSN ile boşluk doldurma. Bozuk kayıt hattı durdurmaz, DLQ'ya gider. |
| `merger` | Raporu mevcut olaylarla eşleştirir; bağlar ya da yeni olay açar. Tek tüketici (bkz. "Neden tek merger?"). |
| `notifier` | Etkilenen aboneleri `ST_DWithin` ile bulur; Telegram hız sınırlarına uyarak gönderir. |
| `summarizer` | M ≥ 3,5 olaylar için özet yazar. Sayıları kod üretir, model yalnızca cümle kurar, doğrulayıcı her sayıyı denetler. |
| `api` | REST + WebSocket, gömülü Leaflet harita. |
| `replay` | Bir deprem dizisini hızlandırarak oynatıp doğruluk ve gecikmeyi ölçer. |

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

| Eşikler | F1 | F1 (2× gürültü) |
|---|---|---|
| Plan §8: 60 sn / 100 km / 0,6 | 0,847 | 0,851 |
| **Seçilen: 10 sn / 50 km / 0,5** | **0,959** | **0,954** |

> ⚠️ Ayar **sentetik** bir diziyle yapıldı: kaynakların gerçek kataloglarına
> geliştirme ortamından erişilemedi. Sentetik dizide her raporun hangi depreme
> ait olduğu bilindiği için doğruluk kesin ölçülür; ama kaynaklar arası
> farkların dağılımı benim varsayımım. Gerçek bir katalogla yeniden doğrulanmalı.
> Ayrıntı: [docs/MATCHING.md → Eşiklerin ayarı](docs/MATCHING.md#eşiklerin-ayarı).

## Ölçüm sonuçları

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

Gerçek bir afetin verisini kullanırken bunu saygıyla ve yalnızca mühendislik
amacıyla yapıyorum; sentetik dizi Sındırgı'nın yalnızca konumunu ve büyüklüğünü kullanır.

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
ARTCI_SWEEP=1 go test ./internal/merger -run Sweep -v           # eşik taraması
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

> ⚠️ Ayrıştırıcı testlerinin girdileri ([`testdata/source`](testdata/source/README.md))
> kaynakların bilinen biçimine göre elle hazırlandı; geliştirme ortamının bu
> adreslere erişimi yoktu. Gerçek yanıtlarla değiştirilmeli.

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
  replay/     sentetik dizi, doğruluk ve yüzdelik hesapları
  geo/ api/ metrics/ app/
migrations/   SQL göçleri (servislere gömülü)
testdata/     ayrıştırıcı girdileri, red-team, eşleştirme örnekleri
web/          Leaflet harita (api'ye gömülü)
deploy/       compose (dev + canlı), Caddyfile, Prometheus, Grafana
```
