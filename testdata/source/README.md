# testdata/source

Ayrıştırıcı testlerinin girdileri.

**Uyarı:** Bu dosyalar kaynakların bilinen biçimine göre **elle hazırlanmıştır**:
AFAD apiv2 JSON dizisi, Kandilli `lst0.asp` sayfasındaki `<pre>` tablosu,
EMSC SeismicPortal FDSN GeoJSON yanıtı ve WebSocket mesajları. Geliştirme
ortamının bu adreslere ağ erişimi yoktu. Plan §5'e göre bunlar gerçek
yanıtlarla değiştirilmeli:

```sh
curl -A "artci (iletisim: ...)" 'https://deprem.afad.gov.tr/apiv2/event/filter?start=...&end=...&format=json' > afad.json
curl -A "artci (iletisim: ...)" 'http://www.koeri.boun.edu.tr/scripts/lst0.asp' > kandilli.html   # Windows-1254 olabilir
curl -A "artci (iletisim: ...)" 'https://www.seismicportal.eu/fdsnws/event/1/query?format=json&limit=20&minlat=34&maxlat=43.5&minlon=24&maxlon=46' > emsc_fdsn.json
```

Testler, bilerek bozuk bırakılmış kayıtlara (DLQ'ya gitmesi gereken) dayanır;
gerçek yanıtlarla değiştirirken bu satırlar korunmalı.
