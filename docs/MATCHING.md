# Tekilleştirme: aynı depremi tanımak

Plan §8'in uygulanmış hâli. Kod: `internal/match` (saf karar mantığı),
`internal/merger` (akış ve transaction), `internal/store/reports.go` (SQL).

## Akış

Her `reports.raw` mesajı için **tek bir transaction**:

```
UpsertReport ──► değişmedi ─────────────────────────────► noop
     │
     ├──► güncellendi ─► hâlâ olayına uyuyor mu?
     │                     ├─ evet ─► olayı yeniden hesapla ─► update
     │                     └─ hayır ─► kopar, eski olayı yeniden hesapla,
     │                                 raporu yeniden eşleştir ─► update
     └──► yeni ─► adayları bul ─► Decide ─┬─ Attach ─► bağla, yeniden hesapla ─► attached
                                          └─ NewEvent ─► olay aç ─► new_event
COMMIT ─► yayınlanmamış sürümleri events.updated'a yaz
```

Kararlar `artci_merge_decisions_total{decision}` metriğinde sayılır.

## 1. Aynı rapor mu? (idempotency)

`reports` tablosundaki `UNIQUE (source, source_event_id)` ve:

```sql
INSERT ... ON CONFLICT (source, source_event_id) DO UPDATE SET ...
WHERE <anlamlı bir alan değişti>
RETURNING id, event_id, (xmax = 0) AS inserted
```

- Satır dönmezse: hiçbir şey değişmedi → **noop**. Polling her 30 sn'de aynı
  kaydı getirir; bu, sistemin en sık yolu.
- `xmax = 0` ise satır yeni eklendi; değilse güncellendi.
- `received_at` ve `raw` "anlamlı" sayılmaz; `received_at` hiç güncellenmez
  (raporun bize **ilk** ulaştığı an gecikme ölçümü için gerekli).
- Büyüklük/derinlik `NUMERIC(…,1)`, zaman mikro saniye: Go tarafındaki
  karşılaştırmalar (`match.ReportChanged`, `match.EventChanged`) aynı
  hassasiyette yapılır; yoksa DB gidiş-dönüşü sahte "değişiklik" üretir.

## 2-3. Aday ve puan

Aday: `origin_time ±W` **ve** `ST_DWithin(location, nokta, R)`. Plan §8'de
W = 60 sn, R = 100 km idi; replay ölçümünden sonra **W = 10 sn, R = 50 km**
(bkz. "Eşiklerin ayarı"). Aşağıdaki formül Plan değerleriyle yazılmıştır;
kodda ölçekler pencereyle birlikte değişir.
Zaman B-tree, mesafe GIST indeksini kullanır.

```
puan = 1 − ( |Δt| / 60 s × 0,4  +  mesafe / 100 km × 0,4  +  |ΔM| / 1,0 × 0,2 )
```

Mesafe `geo.HaversineKm`. Puan, olayın **gösterilen** değerlerine göre hesaplanır.
Pencere dışı aday puanı ne olursa olsun elenir (Δt = 70 sn, mesafe 0 olsa bile
puan ~0,53 çıkar; pencere ayrıca garanti eder).

Örnek (Plan §1): Kandilli raporunun AFAD olayına puanı
`1 − (1/60×0,4 + 2,8/100×0,4 + 0,2×0,2) ≈ 0,94`.

## 4-5. Karar

En yüksek puanlı aday **≥ 0,6** ise ve **aday olayda bu kaynaktan başka rapor
yoksa** bağlanır; yoksa yeni olay. Eşit puanda küçük `EventID` seçilir (aday
sırası sonucu değiştirmesin).

**Aynı kaynak kuralı ve istisnası.** Bir kaynak aynı depremi iki kez
raporlamaz; o yüzden aynı kaynaktan ikinci rapor genellikle *başka bir deprem*
demektir (artçı dizisi: 20 sn arayla, 15 km ötede iki deprem puanda ~0,8 alır
ama kural onları ayırır). Ancak kaynaklar bazen aynı depremi iki farklı id ile
yayınlar. Bunu ayırt etmek için çok daha sıkı bir "neredeyse aynı" sınırı
kullanılır: aynı kaynağın mevcut raporuyla **Δt ≤ 5 sn, mesafe ≤ 10 km,
|ΔM| ≤ 0,5** ise bağlanır.

## 6. Gösterilen değerler (`match.Resolve`)

Öncelik **AFAD > Kandilli > EMSC**; eşit öncelikte **en son alınan** rapor
kazanır. `Sources` öncelik sırasında, `FirstReportedAt` en erken `received_at`.
`Resolve` yalnızca rapor **kümesine** bakar, geliş sırasına bakmaz — sıra
bağımsızlığının temeli budur.

Olay yalnızca gösterilen bir değer (kaynak listesi dahil) değiştiyse yazılır;
o zaman `version` +1 olur ve olay yayınlanır. Örn. AFAD varken EMSC'nin
büyüklük düzeltmesi `update` kararıdır ama olay sürümü artmaz, yayın olmaz.

> "Kullanıcıya yeniden bildirim gitsin mi?" ayrı bir sorudur; o eşik
> (ΔM ≥ 0,5) notifier'dadır ve `EventUpdate.PrevMagnitude` ile hesaplanır.

## Olay bölünmesi (güncelleme raporu olayından koparınca)

Kaynak konumu 200 km düzeltirse rapor artık olayına ait değildir. Kural:

1. Raporun olayında **başka rapor yoksa**: olay raporla birlikte taşınır (basit güncelleme).
2. Başka raporlar varsa, güncellenen rapor *diğerlerinden* hesaplanan olaya
   `Decide` ile yeniden sınanır. Uymuyorsa:
   - rapor olaydan koparılır,
   - eski olay kalan raporlarla yeniden hesaplanır (sürüm artar, yayınlanır),
   - rapor yeni gelmiş gibi yeniden eşleştirilir (başka bir olaya bağlanabilir
     ya da yeni olay açar).

Neden olayı silip yeniden kurmuyoruz? Olay kimliği bildirimlerin
(`notifications`) ve özetlerin anahtarı; kimliği korumak "aynı deprem için
ikinci bildirim" riskini önler. Bedeli: bölünen yeni olay için abonelere yeni
bildirim gidebilir — doğru davranış, çünkü gerçekten ayrı bir deprem.

## Neden tek merger? (Plan §6)

Eşleştirme "oku → karar ver → yaz" işlemidir. İki merger aynı depremin iki
raporunu aynı anda işlerse ikisi de aday bulamaz ve **iki olay** açar (race
condition). MVP'de tek tüketici (`group=merger`, sabit ad `merger-1`) bunu
yapısal olarak engeller; hacim (dakikada birkaç rapor) için fazlasıyla yeter.
Ölçek gerekirse: bölgeye göre partition (her bölge tek merger'da) ya da aday
olayları `SELECT … FOR UPDATE` / advisory lock ile kilitlemek.

## Teslim garantisi

- Mesaj **at-least-once** gelir; upsert'in no-op'u tekrarı etkisiz kılar.
- Yayın commit'ten **sonra** yapılır. Arada çökme olursa yeniden işlenen mesaj
  noop görünür ve yayın kaybolurdu; bunu `events.published_version` (002
  migration, küçük bir outbox) önler: her mesajdan sonra ve açılışta
  `published_version < version` olan olaylar yayınlanır.
- Çözülemeyen/geçersiz mesaj ve kalıcı DB hatası (SQLSTATE 22/23) → DLQ + ACK.
  Geçici hata → ACK yok, mesaj yeniden denenir.

## Bilinen sınırlar

- **Çapraz artçılar.** Eşleştirme açgözlüdür (gelen raporu o anki olaylara
  bakarak yerleştirir). İki yakın deprem için önce Kandilli-1, sonra AFAD-2
  gelirse AFAD-2 1. depremin olayına bağlanır (tek geçerli aday). Olay sayısı
  doğru kalır ama üyelik çaprazlanabilir; 24 sıralamanın 8'inde görülür
  (`TestOrderIndependence_AftershockPermutations`). Tek deprem için sonuç
  tüm sıralamalarda aynıdır. Çözüm yeniden kümeleme (olay değişince
  komşularını yeniden değerlendirmek) — replay ölçümü gerekli gösterirse.
- **Eski güncellemenin geç gelmesi.** Kaynak kayıtlarında sürüm bilgisi yok;
  4,1 düzeltmesi 4,3'ten önce işlenirse son yazılan (4,3) kalır.
- **Eşikler iki kaynaklı gerçek veriyle doğrulandı.** Sentetik veriyle
  seçildi, gerçek Sındırgı dizisinin AFAD ve Kandilli çözümleriyle doğrulandı
  (bkz. "Gerçek dizide doğrulama"). EMSC ISC bülteninde olmadığı için gerçek
  veride sınanmadı. Ortamdan: `ARTCI_MATCH_WINDOW`, `ARTCI_MATCH_RADIUS_KM`,
  `ARTCI_MATCH_MIN_SCORE`.

## Eşiklerin ayarı

Plan §8'deki 60 sn / 100 km / 0,6 başlangıç tahminiydi. `replay` aracıyla
ölçüldü ve değiştirildi.

### Veri

Gerçek bir artçı kataloğuna (kaynakların aynı döneme ait kayıtları) bu
geliştirme ortamından erişilemedi. Bunun yerine **sentetik** bir dizi
kullanıldı (`internal/replay/gen.go`, `replay gen`):

- M6,1 ana şok (Sındırgı konumu), 24 saatte 400 artçı; zamanlar değiştirilmiş
  Omori yasasıyla (c = 10 dk, p = 1,1), büyüklükler Gutenberg-Richter ile (b = 1).
- Her kaynak (AFAD, Kandilli, EMSC) her depremi kendi tamlık olasılığı,
  gecikmesi (1–8 dk) ve ölçüm farklarıyla raporlar: oluş zamanı ±0,8–1,5 sn,
  konum ±2,5–4 km, büyüklük ±0,12–0,20 (1σ); bir kısmı sonradan revize edilir.
- Ana şoktan sonraki ilk dakikalarda küçük depremler kaçırılır (kısa vadeli eksiklik).

Sentetik verinin üstünlüğü: her raporun hangi gerçek depreme ait olduğu
bilinir, doğruluk kesin ölçülür. Zayıflığı: kaynaklar arası farkların
dağılımı benim varsayımımdır. Bu yüzden tarama **iki gürültü düzeyinde**
(varsayılan ve 2 katı) yapıldı ve sıralama ikisinin ortalamasına göre
yapıldı; yalnızca varsayılan gürültüye iyi oturan eşik seçilmesin diye.

### Ölçüt

Çift tabanlı precision/recall (`internal/replay/metrics.go`): farklı
kaynaklardan gelen her rapor çifti için "aynı deprem mi?" ile "aynı olaya mı
bağlandı?" karşılaştırılır. Düşük precision = farklı depremler birleşiyor;
düşük recall = aynı deprem birden çok olay olarak görünüyor (mükerrer bildirim).

### Tarama

```
ARTCI_SWEEP=1 go test ./internal/merger -run Sweep -v
```

Bellek içi merger ile 217 eşik bileşimi × 2 gürültü × 3 tohum. Bellek içi
sonuç, aynı katalogla PostGIS + Redis üzerinde çalışan gerçek merger'la
birebir aynı çıktı (precision 0,8991 / recall 0,9469, iki yolda da).

| Eşikler (pencere / yarıçap / eşik / aynı-kaynak) | F1 (1×) | F1 (2× gürültü) | Ortalama |
|---|---|---|---|
| **Plan §8:** 60 sn / 100 km / 0,6 / 5 sn | 0,847 | 0,851 | 0,849 |
| **Seçilen:** 10 sn / 50 km / 0,5 / 2 sn | 0,959 | 0,954 | **0,956** |
| 15 sn / 50 km / 0,6 / 2 sn | 0,962 | 0,948 | 0,955 |
| 5 sn / 30 km / 0,5 (yalnız 1×'te en iyi) | 0,982 | < 0,92 | — |

Okuma:

- **Asıl sorun zaman penceresiydi.** Aynı depremin kaynaklar arası Δt'si
  neredeyse hep birkaç saniye; 60 sn'lik pencerede ise yoğun dizide 3–10 farklı
  deprem aday oluyordu. İlk koşuda yanlış birleşen çiftlerin medyan Δt'si
  8 sn, doğru çiftlerinki 0,9 sn idi.
- **5 sn penceresi aşırı uyum.** Varsayılan gürültüde en iyisi ama gürültü
  iki katına çıkınca listeden düşüyor. Gerçek kurumların farkları benim
  varsayımımdan büyükse ilk bozulacak seçenek bu.
- **Aynı-kaynak istisnası** (0 / 2 / 5 sn) sonucu neredeyse hiç
  değiştirmedi; Plan §8'deki "aynı depremi iki id ile yayınlama" durumu
  için 2 sn ile korundu.
- **Yoğunluk sınırı.** Aynı ayarla 10 kat yoğun bir dizide (günde 4000
  artçı) F1 0,70'e düşüyor. Açgözlü eşleştirme saniyeler arayla olan
  depremleri ayıramıyor; bu bölümün başındaki "çapraz artçılar" sınırı.
  Gerekirse çözüm yeniden kümeleme.

## Gerçek dizide doğrulama

Sentetik veriyle seçilen eşikler, gerçek bir artçı dizisiyle doğrulandı.

### Veri: ISC bülteni

AFAD API'si yalnızca son ~10 günü tutuyor; geçmiş bir dizi oradan alınamıyor.
[ISC bülteni](http://www.isc.ac.uk/iscbulletin/) (International Seismological
Centre) ise her deprem için tek bir olay kaydı ve altında kurumların **kendi
çözümlerini** ayrı ayrı tutuyor: `AFAD` ve `ISK` (Kandilli). Bu tam olarak
tekilleştirmenin girdisi ve doğru cevabı:

- her kurum çözümü bir rapor (gerçek zaman, konum, büyüklük farklarıyla),
- ISC olay kimliği `truth`.

```
go run ./cmd/replay isc -out testdata/replay/sindirgi-isc-2025-08.jsonl.gz
```

10 Ağustos 2025 Sındırgı (Mw 6,1) depreminin ilk 7 günü, 38,7–39,7 K /
27,5–28,9 D: **4201 olay, 6949 rapor** (AFAD 3332, Kandilli 3617); 2748
olayda iki kurumun da çözümü var. Büyüklükler 0,7–6,1, medyan 1,8. Bültende
yayın gecikmesi olmadığı için raporların merger'a geliş anı sentetik
üreticideki kaynak gecikme modelinden örneklenir; tarama üç farklı varış
sırasının ortalamasıdır.

Aynı olaydaki AFAD–Kandilli farkları (gerçek):

| | p50 | p90 | p95 | p99 |
|---|---|---|---|---|
| \|Δt\| | 0,6 sn | 1,4 sn | 2,2 sn | 30,8 sn |
| mesafe | 2,5 km | 7,6 km | 10,2 km | 20,5 km |
| \|ΔM\| | 0,2 | 0,4 | 0,5 | 0,7 |

Sentetik modelin varsayımlarıyla (Δt 1σ 0,8–1,0 sn, konum 2,5–3 km) uyumlu;
gerçek dağılımın kuyruğu daha uzun.

**Doğru cevabın sınırı.** Dizinin olayları ISC tarafından henüz
incelenmedi ("Event not reviewed by the ISC"); gruplama otomatik. 45 çiftte
(%1,6) iki çözüm aynı olay sayılmış ama 10 sn / 50 km'den uzak (en uç: 59 sn,
113 km). Bunlar ya ISC'nin yanlış birleştirmesi ya da kurumların gerçekten çok
farklı çözümü; her iki durumda da hiçbir makul pencere onları bağlayamaz. Bu
yüzden ulaşılabilir recall ≈ **0,984**. Tersine, 25 çift ayrı olay sayılmış
ama 3 sn ve 15 km içinde: büyük ihtimalle ISC'nin bölmesi gerekmeyen olaylar,
precision'ı biraz düşük gösterir.

### Sonuç

```
ARTCI_SWEEP=1 go test ./internal/merger -run SweepReal -v
```

| Eşikler | Precision | Recall | F1 (gerçek) | F1 (sentetik) |
|---|---|---|---|---|
| Plan §8: 60 sn / 100 km / 0,6 | 0,945 | 0,942 | 0,944 | 0,847 |
| **Varsayılan: 10 sn / 50 km / 0,5** | **0,991** | **0,981** | **0,986** | **0,959** |
| 10 sn / 50 km / 0,4 (gerçekte en iyi) | 0,991 | 0,983 | 0,987 | 0,951 |
| 5 sn / 50 km / 0,5 | 0,991 | 0,977 | 0,984 | 0,981 |

Okuma:

- **Varsayılan eşikler gerçek veride doğrulandı;** değiştirilmedi. En iyi
  bileşimle farkı 0,0007 (gürültü düzeyinde), ve o bileşim sentetik veride
  daha kötü. 72 bileşimin en iyi 12'si 0,983–0,987 arasında: sonuç eşiklere
  duyarlı değil, düz bir tepe.
- **Plan §8'in geniş penceresi gerçekte de yanlış birleştiriyor:** precision
  0,991 → 0,945. Etki sentetik dizideki kadar büyük değil, çünkü gerçek
  dizinin en yoğun dönemindeki küçük depremlerin bir kısmını kurumlar
  kaydetmemiş.
- **Kalan hatanın çoğu doğru cevaptan.** Recall 0,981, ulaşılabilir tavan
  0,984. Varsayılan ayarla 4201 olaydan %98,2'si tam doğru birleştirildi.
- **5 sn penceresi** gerçek veride de iyi (0,984) ama Δt'nin p99'u 30 sn;
  daha dar pencere kuyruğu daha çok keser. 10 sn korundu.

`TestTunedConfigOnRealSequence` her `go test`'te bu kataloğu oynatır;
F1 0,98'in ya da precision 0,985'in altına düşerse kırılır.
