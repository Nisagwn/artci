# Artçı — Gerçek Zamanlı Deprem Veri Hattı

> **Bu bir eğitim projesidir; resmî bilgi için [AFAD](https://deprem.afad.gov.tr)'ı takip edin.**

Artçı, AFAD, Kandilli Rasathanesi ve EMSC'den gelen deprem raporlarını
ortak bir olay modelinde birleştiren, Go ile geliştirilmiş olay güdümlü bir
veri işleme sistemidir. Depremleri canlı haritada gösterir, il bazlı Telegram
abonelikleri üzerinden bildirim gönderir ve M ≥ 3,5 olaylar için kaynak
verileriyle tutarlılığı denetlenen Türkçe özetler üretir.

Go · PostgreSQL + PostGIS · Redis Streams · Prometheus + Grafana · Docker Compose + Caddy

**Dokümantasyon:** [Proje planı](PLAN.md) · [Eşleştirme yöntemi](docs/MATCHING.md) · [Bildirim politikası](docs/NOTIFY.md)

**İçindekiler:** [Özellikler](#özellikler) · [Mimari](#sistem-mimarisi) · [Tekilleştirme](#tekilleştirme) · [Ölçümler](#ölçüm-sonuçları) · [Kurulum](#çalıştırma) · [Veri kaynakları](#veri-kaynakları)

## Özellikler

- **Yer arama:** il veya yer adına göre listeyi ve haritayı süzer; eşleşen sonuçlara haritada yaklaşır. `/` aramaya geçer, `Esc` aramayı temizler.
- **Filtreler:** son 1 saat, 24 saat veya 7 gün; her büyüklük ya da 2, 3, 4 ve üstü. Liste en yeni veya en büyük depreme göre sıralanır. Seçilen aralıkta API'den en fazla 2000 olay yüklenir.
- **Canlı görünüm:** harita ve liste WebSocket ile güncellenir; bağlantı koparsa yeniden bağlanır. Saatler Türkiye saatidir; açık/koyu görünüm sistem tercihini izler.
- **Telegram bağlantısı:** bot kullanıcı adı ayarlıysa “Telegram'da bildirim al” bağlantısı görünür. Aramaya tanınan bir il adı yazıp bağlantıyı açarak Telegram'da botu başlatmak, o il için varsayılan aboneliği oluşturur. İl tanınmazsa bot karşılama mesajını gösterir.

### Telegram abonelikleri

Bot, il adıyla abonelik oluşturmayı ve düğmeler üzerinden abonelik yönetimini
destekler. `İzmir` mesajı, il merkezinin **100 km** çevresindeki **M ≥ 3,5**
depremler için abonelik oluşturur. `İzmir 50 3` mesajı ise yarıçapı 50 km,
asgari büyüklüğü 3 olarak ayarlar.

**Aboneliklerim**, **Aboneliği bitir** ve **Yardım** düğmelerinin yanında
`/abone`, `/aboneliklerim`, `/iptal` ve `/yardim` komutları da desteklenir.
Aynı il için yeniden başvuru mevcut aboneliği günceller. Sohbet başına en
fazla 5 il eklenebilir; iptal işlemi o sohbetin tüm aboneliklerini siler.

Aboneye olay başına en fazla bir ilk bildirim ve bir güncelleme gönderilir.
Güncelleme, büyüklük hâlâ abonelik eşiğinin üstünde veya ona eşitken,
**kullanıcıya bildirilen değerden en az 0,5 farklıysa** gönderilir.
Kararların ayrıntısı: [docs/NOTIFY.md](docs/NOTIFY.md).

## Problem

Aynı deprem, kaynakların yayın zamanları ve ölçüm yöntemleri nedeniyle
farklı saat, büyüklük ve konum değerleriyle raporlanabilir. Örnek:

| Kaynak | Saat | Büyüklük | Konum |
|---|---|---|---|
| AFAD | 15:04:31 | 4,1 | 39,21 K · 28,15 D |
| Kandilli | 15:04:30 | 4,3 | 39,19 K · 28,17 D |
| EMSC | 15:04:32 | 4,2 | 39,23 K · 28,12 D |

Amaç, aynı depreme ait raporları tek olay altında toplamak ve yinelenen
bildirimleri önlemektir. Buna karşılık, yoğun artçı dizilerinde saniyeler
arayla gerçekleşen farklı depremlerin ayrı tutulması gerekir.

## Sistem mimarisi

Artçı, ayrı süreçler olarak çalışan Go servislerinden oluşur. Servisler
arasındaki veri aktarımı `reports.raw` ve `events.updated` Redis akışları
üzerinden yürütülür; kalıcı kayıtlar PostgreSQL'de tutulur. Kaynak
istemcilerinin ayrı süreçlerde çalışması, tek bir kaynaktaki kesintinin
diğer kaynakların veri toplamasını durdurmasını önler.

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
| `notifier` | `events.updated`, `subscriptions` | `notifications`, Telegram | Etkilenen aboneleri `ST_DWithin` ile bulur; Telegram hız sınırlarına uyarak gönderir. Abone ve olay başına en fazla bir `initial` ve bir `update`; aynı türün tekrarını birincil anahtar engeller. |
| `summarizer` | `events.updated` | `summaries`, `llm_spend` | M ≥ 3,5 olaylar için özet yazar. Sayıları kod üretir, model yalnızca cümle kurar, doğrulayıcı her sayıyı denetler. |
| `api` | `events`, `events.updated` | WebSocket istemcileri | REST + WebSocket, gömülü Leaflet harita. |
| `replay` | katalog (JSONL), ISC bülteni | `reports.raw` | Sentetik ya da gerçek (ISC) bir deprem dizisini hızlandırarak oynatıp doğruluk ve gecikmeyi ölçer. |

### Veri işleme akışı

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
        text kind PK "initial | update"
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

1. `(source, source_event_id)` mevcutsa rapor **güncelleme** olarak işlenir; içerik değişmemişse işlem sonlandırılır (idempotent).
2. Zaman farkı ≤ **10 sn** ve mesafe ≤ **50 km** olan olaylar adaydır.
3. `puan = 1 − (|Δt|/10 sn × 0,4 + mesafe/50 km × 0,4 + |ΔM| × 0,2)`
4. En iyi puan ≥ **0,5** ise ve aday olayda **aynı kaynaktan başka rapor yoksa** rapor bu olaya bağlanır; aksi hâlde yeni olay oluşturulur.
5. Gösterilen değerler AFAD > Kandilli > EMSC önceliğiyle seçilir. Değerler değiştiğinde `version` artırılır ve `events.updated` akışına yazılır.

### Eşik seçimi ve doğrulama

Başlangıç planındaki eşikler 60 sn / 100 km / 0,6 olarak belirlenmiştir.
Sentetik artçı dizilerinin `replay` ile değerlendirilmesinde, yoğun dönemlerde
60 saniyelik pencerede 3–10 farklı depremin aday hâline geldiği görülmüştür.
Yanlış birleştirilen çiftlerin medyan zaman farkı 8 sn, doğru çiftlerinki
0,9 sn'dir. İki gürültü düzeyinde 217 eşik bileşimi taranarak her iki
koşulda da tutarlı sonuç veren eşikler seçilmiştir.

Seçilen eşikler, 10 Ağustos 2025 Sındırgı (Mw 6,1) depremini izleyen ilk
7 güne ait gerçek kayıtlarla da değerlendirilmiştir. [ISC bültenindeki](http://www.isc.ac.uk/iscbulletin/)
AFAD ve Kandilli çözümlerinden oluşan veri kümesi 4201 deprem ve 6949 rapor
içerir. Değerlendirmede ISC'nin olay gruplaması referans alınmıştır.

| Eşikler | F1 sentetik | F1 sentetik (2× gürültü) | **F1 gerçek dizi** |
|---|---|---|---|
| Plan §8: 60 sn / 100 km / 0,6 | 0,847 | 0,851 | 0,944 |
| **Seçilen: 10 sn / 50 km / 0,5** | **0,959** | **0,954** | **0,986** |

Gerçek dizide seçilen eşiklerin F1 değeri, taranan 72 bileşimdeki en iyi
sonucun 0,0007 altındadır; mevcut eşikler korunmuştur. Referans veri kümesi
de sınırlamalar içerir: ISC'nin otomatik gruplamasındaki 45 çift, 10 sn /
50 km aday penceresinin dışındadır. Bu pencereyle ulaşılabilir recall
yaklaşık 0,984, ölçülen değer ise 0,981'dir. Ayrıntı:
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

10× hızdaki ölçümde 6,9 saniyelik bir uç değer gözlenmiştir. İlgili olayın
ilk raporu 195,4. saniyede alınmış, olay kimliği ilk kez 202,3. saniyede
yayımlanmıştır. İnceleme, raporun önce başka bir olayla eşleşip sonradan
ayrı bir olaya taşınmış olabileceğini göstermektedir; ölçüm bu süreyi
gecikmeye dahil eder.

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
oynatılır; F1 değeri 0,98'in altına düşerse test başarısız olur. Aynı katalog
gerçek hattan da geçirilebilir (`make replay-real`, merger + PostGIS + Redis
çalışıyor olmalı): ölçülen F1 0,9857, bellek içi testle aynı.

Gerçek olay verileri yalnızca mühendislik değerlendirmesi amacıyla
kullanılır. Veri kümesi, kurumların yayımladığı deprem parametreleriyle
(zaman, konum ve büyüklük) sınırlıdır.

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

Sistem, Redis Streams üzerinde **at-least-once teslim ve idempotent işlem**
modelini kullanır. Tüketici işlemi tamamlayıp ACK göndermeden kapanırsa mesaj
yeniden teslim edilebilir. Yinelenen raporlar `UNIQUE` kısıtı ve upsert ile
etkisiz hâle getirilir; aynı bildirim türünün tekrar kaydedilmesi
`notifications` tablosunun birincil anahtarıyla engellenir.
Ancak kayıt ile gönderim arasında süreç çökerse bildirim kaybolabilir;
Telegram gönderimiyle veritabanı kaydı tek transaction değildir. Bu nedenle
uçtan uca “tam olarak bir kez” garantisi yoktur. Ayrıntı:
[bildirim gönderim sırası](docs/NOTIFY.md#tek-seferlik-gönderim-kaydet--gönder--başarısızsa-sil).

### Neden tek merger?

Eşleştirme "oku → karar ver → yaz" işlemidir. İki merger aynı depremin iki
raporunu aynı anda işlerse ikisi de aday bulamaz ve iki olay açar. Hacim
(sakin dönemde dakikada birkaç rapor, artçı fırtınasında saniyede birkaç)
tek tüketiciyle karşılanır: ölçülen iş hacmi saniyede yaklaşık 220–350 rapordur.
Daha yüksek hacimler için bölgesel bölümleme veya aday olayların kilitlenmesi
gibi yöntemlerin değerlendirilmesi gerekir.

### Neden Kafka değil?

Mevcut iş yükü için gereken tüketici grupları, bekleyen mesaj takibi ve
yeniden işleme özellikleri Redis Streams ile karşılanır. Kafka'nın bölümleme
ve uzun süreli olay saklama özellikleri bu kurulumun gereksinimleri arasında
olmadığından, işletim karmaşıklığını sınırlamak için Redis tercih edilmiştir.

## Yapay zekâ destekli özetler ve doğrulama

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

### İsteğe bağlı LLM ayarları

`ARTCI_LLM_API_KEY` boşsa hat yalnızca şablonlu özetlerle çalışır.
`ARTCI_LLM_MODEL` tek model adı veya **OpenRouter için virgülle ayrılmış
yedek model listesi** kabul eder. Ücretsiz modeller de kullanılabilir;
örnek model listesi [deploy/.env.example](deploy/.env.example) içindedir.
Sağlayıcının model erişimi ve kullanım sınırları geçerlidir; model hatası
olduğunda şablona dönülür.

Model adı boş bırakılırsa uygulamadaki varsayılan
`anthropic/claude-haiku-5.5` kullanılır; bu ücretsiz model seçimi değildir.
`ARTCI_LLM_DAILY_USD` günlük harcama tavanıdır (varsayılan `1`).
OpenRouter isteklerinde düşünme kapatılır ve yapılandırılmış JSON yanıtı istenir.

## Gözlem

Prometheus metrikleri ([`internal/metrics`](internal/metrics/metrics.go)),
Grafana panosu ([`deploy/grafana/dashboards/artci.json`](deploy/grafana/dashboards/artci.json)),
alarmlar ([`deploy/prometheus/alerts.yml`](deploy/prometheus/alerts.yml)):

- Bir kaynağa 15 dakikadır ulaşılamaması; sakin dönemlerde yanlış alarmı azaltmak için rapor sayısı yerine kaynak erişimi izlenir.
- DLQ son 1 saatte 10'dan fazla kayıt aldı
- Bildirim gecikmesi p95 > 30 sn
- LLM özetlerinin %20'sinden fazlası reddediliyor

## Çalıştırma

### Gereksinimler

- Go 1.26.7 veya üzeri (`go.mod` ile uyumlu).
- Docker ve Docker Compose; yerel PostGIS/Redis altyapısı veya tüm servislerin çalıştırılması için.
- Make hedefleri için GNU Make ve POSIX uyumlu kabuk; Windows'ta WSL veya uygun bir kabuk ortamı kullanılabilir.
- Telegram ve LLM entegrasyonları için ilgili sağlayıcı anahtarları; her iki entegrasyon da isteğe bağlıdır.

### Yerel geliştirme

Altyapıyı başlatın:

```sh
make dev-up
```

Ardından aşağıdaki hedeflerin her birini ayrı terminalde çalıştırın.
Makefile, yerel veritabanı ve Redis bağlantılarını ayarlar; her servis için
ayrı metrik portu kullanarak port çakışmalarını önler.

```sh
make run-merger
make run-ingest-afad
make run-ingest-kandilli
make run-ingest-emsc
make run-summarizer
make run-notifier
make run-api
```

Harita [http://localhost:8080](http://localhost:8080) adresinde açılır.
Telegram token'ı verilmediyse bildirici yalnızca log üretir; LLM anahtarı
verilmediyse özetleyici şablon kullanır. İsteğe bağlı ayarları servisleri
başlatmadan önce ortam değişkeni olarak tanımlayın. `deploy/.env` dosyası
yerel `go run` komutları tarafından kendiliğinden yüklenmez.

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
# deploy/.env dosyasındaki alan adı, parola ve isteğe bağlı bot/LLM ayarlarını düzenleyin.
make up    # = docker build -t artci:latest . && docker compose ... up -d --no-build --wait
```

Temel yapılandırma [deploy/.env.example](deploy/.env.example) dosyasında yer alır:

| Değişken | Açıklama |
|---|---|
| `ARTCI_DOMAIN` | Caddy'nin hizmet vereceği alan adı; yerel HTTP denemesi için `http://localhost`. |
| `POSTGRES_PASSWORD` | PostgreSQL parolası. Örnek değer değiştirilmelidir. |
| `GRAFANA_ADMIN_PASSWORD` | Grafana yönetici parolası. Örnek değer değiştirilmelidir. |
| `ARTCI_USER_AGENT` | Veri kaynaklarına gönderilen uygulama ve iletişim bilgisi. |
| `ARTCI_TELEGRAM_TOKEN` | Bot token'ı; boşsa bildirimler yalnızca loglanır. |
| `ARTCI_TELEGRAM_BOT` | Aynı botun `@` olmadan kullanıcı adı; boşsa sitedeki abonelik bağlantısı gizlenir. |
| `ARTCI_LLM_API_KEY` | LLM sağlayıcı anahtarı; boşsa şablonlu özet kullanılır. |
| `ARTCI_LLM_MODEL` | Model adı veya OpenRouter için virgülle ayrılmış yedek model listesi. |
| `ARTCI_LLM_DAILY_USD` | Günlük LLM harcama tavanı; varsayılan `1` USD. |

Ayarlar değiştirildiğinde `make up` komutu servisleri güncel yapılandırmayla
yeniden oluşturur.

`make up`, ortak `artci:latest` imajını bir kez derler ve servisleri
`--no-build` ile başlatır. Bu sıra, aynı imajı kullanan yedi servisin
eşzamanlı derlenmesinden kaynaklanabilen Docker Desktop imaj çakışmasını önler.

Yerelde denemek için `ARTCI_DOMAIN=http://localhost` kullanın: Caddy TLS'siz
http://localhost üzerinden sunar. Yalnızca `localhost` yazılması, Caddy'nin
yerel sertifikasını kullanmasına yol açar; tarayıcıda bu sertifikaya güven
tanımlanmamışsa sertifika hatası görülebilir.

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

- **Kullanım kapsamı:** eğitim projesi olduğu site ve bot üzerinde açıkça belirtilir; resmî bilgi için AFAD'a yönlendirilir.
- **İçerik ilkeleri:** özet doğrulayıcısı deprem tahmini, doğrulanamayan hasar bilgisi ve panik ifadelerini reddeder.
- **Veri minimizasyonu:** abonelikler kullanıcının kişisel konumu yerine seçilen ilin merkezini esas alır. `/iptal`, sohbetin aboneliklerini ve bunlara bağlı bildirim kayıtlarını siler.
- **Kaynak kullanımı:** sorgu aralıkları sınırlandırılır, değişmeyen raporlar yeniden yayımlanmaz ve isteklerde açık bir `User-Agent` kullanılır.

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
