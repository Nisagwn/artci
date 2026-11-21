# PostGIS yedekleme ve kurtarma

Yedek; şemayı, PostGIS verisini, deprem ve kaynak raporlarını, Telegram
aboneliklerini ve gönderim kayıtlarını kapsar. Kurtarma noktası son başarılı
arşivin alındığı andır. Redis akışlarını ve `deploy/.env` içindeki uygulama
ayarlarını ve anahtarları ayrı bir kurtarma süreciyle koruyun.

## Şifreli yedek al

Linux veya WSL üzerinde Bash, Docker Compose ve `age` gerekir. Yedek dizinini
aynı dosya sisteminde hard link oluşturulabilen bir yerde tutun. Kurtarma
anahtarını yedeklerden ayrı, erişimi kısıtlı bir konumda oluşturun:

```sh
umask 077
age-keygen -o /guvenli/artci-recovery.agekey
```

Yedekleme makinesine `age-keygen` çıktısındaki açık alıcıyı (`age1...`)
verin. Üretim Compose dosyası varsayılan olarak `deploy/.env` ile okunur.
Farklı bir kurulum için `ARTCI_ENV_FILE` ve `ARTCI_COMPOSE_PROJECT` ayarlayın.

```sh
bash deploy/postgres-backup.sh backup "$AGE_RECIPIENT" \
  "/yedekler/artci-$(date -u +%Y%m%dT%H%M%SZ).age"
```

`pg_dump` çıktısı doğrudan `age` ile şifrelenir. Komut başarıyla tamamlanınca
arşiv hedef adla görünür. Başarılı arşivleri sunucudan bağımsız bir konuma da
saklayın.

## Ayrı bir PostGIS örneğine geri yükle

Kaynakla aynı Postgres ve PostGIS sürümünde bir kurtarma konteyneri hazırlayın.
Aşağıdaki örnek ayrı bir Compose projesinde yalnızca PostGIS'i başlatır.
`/guvenli/kurtarma.env` için `deploy/.env.example` anahtarlarını ve kurtarma
örneğine özel parolaları kullanın. `artci-recovery` için daha önce
kullanılmamış bir proje adı seçin.

```sh
compose=(docker compose --env-file /guvenli/kurtarma.env \
  -f deploy/docker-compose.yml --project-name artci-recovery)
"${compose[@]}" up -d --wait postgres
container=$("${compose[@]}" ps -q postgres)

bash deploy/postgres-backup.sh restore "$container" artci_recovery \
  /guvenli/artci-recovery.agekey /yedekler/artci-20261121T120000Z.age
```

Arşivi kendi yedeklerinizden seçin. `restore`, `template0` üzerinden yeni bir
veritabanı oluşturur ve arşivi tek transaction içinde yükler. Hedef ad zaten
kullanımdaysa işlem durur. Çözme ya da yükleme başarısız olursa araç bu
denemede oluşturduğu veritabanını kaldırmaya çalışır. [PostgreSQL 16 `pg_restore`](https://www.postgresql.org/docs/16/app-pgrestore.html)

Geri yükleme sonunda PostGIS sürümünü ve beklenen veri miktarını kontrol edin:

```sh
docker exec "$container" psql -X -U artci -d artci_recovery -v ON_ERROR_STOP=1 \
  -c 'SELECT postgis_full_version();' \
  -c 'SELECT (SELECT count(*) FROM events) AS events,
             (SELECT count(*) FROM reports) AS reports,
             (SELECT count(*) FROM subscriptions) AS subscriptions,
             (SELECT count(*) FROM notifications) AS notifications;'
```

CI’daki `bash testdata/backup/roundtrip.sh`, sentetik verilerle bu akışın
tamamını ve yanlış anahtarla kurtarma denemesini doğrular.
