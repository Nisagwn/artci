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

Aday: `origin_time ±60 sn` **ve** `ST_DWithin(location, nokta, 100 km)`.
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
- **Eşikler tahmini.** 60 sn / 100 km / 0,6 ve aynı-kaynak 5 sn / 10 km / 0,5
  replay ile ayarlanacak. Ortamdan: `ARTCI_MATCH_WINDOW`,
  `ARTCI_MATCH_RADIUS_KM`, `ARTCI_MATCH_MIN_SCORE`.
