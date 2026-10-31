# Bildirimler: kararlar ve yorumlar

Bu not, `internal/notify` ve `cmd/notifier` yazılırken verilen kararları ve
Plan §10'daki belirsiz ifadenin nasıl yorumlandığını açıklar.

## Plan §10'daki belirsizlik

Plan şöyle diyor:

> Olay ilk kez **min_mag** eşiğini geçtiğinde ... etkilenen aboneler bulunur.
> Güncelleme bildirimi sadece büyüklük **≥ 0,5** değişirse ve olay ilk
> bildirimden sonra eşiği yeni geçtiyse gönderilir.

İkinci cümle tam okunduğunda kendisiyle çelişiyor: bir abone ilk bildirimi
almışsa olay o abonenin eşiğini zaten geçmiştir; "ilk bildirimden sonra eşiği
yeni geçmek" o abone için olamaz. Eşik abonelik başına olduğu için
"eşiği geçmek" de olay başına değil, abone başına bir kavram.

**Yorumum:** iki ayrı durum var ve ikisi de abone başına değerlendirilir.

| Durum | Bildirim türü |
|---|---|
| Abone bu olay için henüz bildirim almadı ve büyüklük ≥ `min_mag` (ilk sürümde ya da sonradan bir düzeltmeyle eşiği ilk kez geçti) | `initial` |
| Abone `initial` aldı, büyüklük hâlâ ≥ `min_mag` ve **kullanıcıya bildirilen** değerden en az 0,5 farklı (artış ya da azalış), daha önce `update` gönderilmedi | `update` |
| Diğer her şey | yok |

"Olay ilk bildirimden sonra eşiği yeni geçtiyse" ifadesini, olayın daha önce
bildirim almamış bir abonenin eşiğini sonradan geçmesi olarak okudum. Bu durum
o abone için bir **ilk** bildirimdir (`initial`), güncelleme değil, çünkü
kullanıcı o deprem hakkında henüz hiçbir şey duymadı.

Sonuçları:

- **Senaryo 2 (4,3 → 4,1):** fark 0,2 < 0,5, bildirim yok (test: `TestNotifierScenario1And2`).
- **4,1 → 4,6:** Ayşe'ye (eşik 3,5) `update` gider. Eşiği 4,5 olan bir aboneye
  ise ilk kez `initial` gider (test: `TestNotifierUpdateAndThresholdCrossing`).
- **Karşılaştırma tabanı olayın önceki sürümü değil, kullanıcıya bildirilen
  değerdir.** `EventUpdate.PrevMagnitude` kullanılmıyor: 4,1 → 4,4 → 4,7 gibi
  küçük adımlarda her adım 0,3 olur ama kullanıcının bildiği değerden toplam
  fark 0,6'dır, bu yüzden güncelleme gitmelidir.
- **Abone başına olay başına en fazla bir `update`.** Şemadaki birincil anahtar
  `(event_id, subscription_id, kind)` bunu zaten zorunlu kılıyor. Tek bir
  deprem için üç dört mesaj göndermek panik dili kadar rahatsız edici olur.
- **Eşiğin altına düşen düzeltme bildirilmez.** `AffectedSubscriptions` yalnızca
  `min_mag <= büyüklük` olan aboneleri döner. Kullanıcı o büyüklükle
  ilgilenmediğini söylemiş. Bedeli şu: 5,0 diye bildirilip 4,0'a düzeltilen bir
  deprem, eşiği 4,5 olan aboneye düzeltme olarak gitmez. Bunu açık bir konu
  olarak bırakıyorum.
- **Ondalık karşılaştırma tam sayıyla yapılır** (`round(m*10)`), böylece
  4,6 − 4,1 = 0,4999… gibi kayan nokta hataları eşiği kaçırtmaz.

## Tek seferlik gönderim: kaydet → gönder → başarısızsa sil

Her abone için sıra şöyle:

1. `RecordNotification`: `INSERT ... ON CONFLICT DO NOTHING`. Satır eklenmediyse
   bu bildirim daha önce gönderilmiş (ya da şu anda başka bir tüketici
   gönderiyor), dolayısıyla gönderilmez. Metrik: `duplicate`.
2. Mesaj hız sınırlı kuyruğa konur, sonucu beklenir.
3. Sonuca göre:
   - **Başarılı:** `sent`. `initial` için `artci_notify_latency_seconds = now − FirstReportedAt`
     ölçülür (`update` için ölçülmez, çünkü ilk rapordan bu yana geçen süre
     gecikmeyi anlatmaz).
   - **Geçici hata** (ağ, 5xx, 429 denemelerinin tükenmesi): kayıt **silinir**,
     işleyici hata döner, akış mesajı ACK'lenmez. Consumer group mesajı yeniden
     teslim ettiğinde başarılı gönderimler `duplicate` olur, yalnızca
     gönderilemeyenler yeniden denenir.
   - **Kalıcı hata** (400/403, ör. kullanıcı botu engelledi): kayıt **silinmez**,
     mesaj ACK'lenir. Silinseydi aynı mesaj sonsuza dek yeniden denenirdi.

Kaydın gönderimden **önce** yapılmasının nedeni: aynı mesaj iki tüketicide
eşzamanlı işlense bile birincil anahtar yalnızca birine "gönder" der. Bedeli,
küçük bir at-most-once penceresi: kayıttan sonra, gönderimden önce süreç
çökerse o bildirim gönderilmemiş olur ama kayıt kalır. Çift bildirim yerine
bunu bilerek seçtim. Hattın geri kalanı at-least-once çalışır, bildirim
günlüğü kullanıcıya görünen etkiyi exactly-once yapar.

## Diğer kararlar

- **Yaş sınırı (`ARTCI_NOTIFY_MAX_AGE`, varsayılan 1 saat):** daha eski
  depremler bildirilmez. Kesintiden sonra biriken mesajlar ya da `replay`
  aracının oynattığı geçmiş olaylar gerçek kullanıcılara "şimdi oldu" diye
  gitmesin diye.
- **Hız sınırı:** genel 25 msg/sn, sohbet başına 1 msg/sn (Telegram'ın ~30 ve
  ~1 sınırlarının altında). Token bucket rezervasyon modelinde çalışır.
  Kuyruğun 4 işçisi var, böylece tek bir sohbetin beklemesi diğerlerini
  tıkamaz. Bot yanıtları da aynı kuyruktan geçer.
- **Mesaj metni:** Plan §4'teki "Size yaklaşık 62 km" yerine
  "Balıkesir merkezine yaklaşık 62 km" yazılıyor. Kişinin konumunu bilmiyoruz,
  yalnızca seçtiği ilin merkezini biliyoruz (Plan §17).
- **"Hissedilmiş olabilir" ifadesi:** büyüklük bantlarına göre kaba bir
  uzaklık tablosu (`feltBands`). Bilimsel bir şiddet modeli değil, yalnızca
  sakin bir cümle seçmek için var. Her zaman "olabilir" kipinde kalır, tahmin
  içermez.
- **Abonelik:** aynı il için tekrar `/abone` yazmak yeni satır açmaz, mevcut
  aboneliği günceller. Sohbet başına en fazla 5 abonelik.
  `/iptal` tüm abonelikleri siler, bildirim günlüğü `ON DELETE CASCADE` ile
  birlikte gider.
- **Dry-run:** `ARTCI_TELEGRAM_TOKEN` yoksa bot kapalıdır ve bildirimler
  yalnızca loglanır, ama günlüğe yine yazılır. Token sonradan verilirse eski
  olaylar yeniden gönderilmez.
- **Token güvenliği:** `net/http` hataları URL'yi, dolayısıyla token'ı içerir.
  İstemci token'ı hata metninden ayıklar.

## Açık konular

- Botu engelleyen (403) kullanıcının aboneliklerini otomatik silmek
  (Plan §17 ile uyumlu olur). Şu an yalnızca loglanıyor.
- Eşiğin altına düşen büyük düzeltmelerin (≥ 0,5) bildirilip bildirilmeyeceği.
- `getUpdates` offset'i bellekte tutuluyor. Yeniden başlatmada son komutlar
  tekrar işlenebilir. Komutlar idempotent olduğu için şimdilik zararsız.
