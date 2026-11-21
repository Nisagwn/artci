#!/usr/bin/env bash
set -euo pipefail
umask 077

root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)
scratch=$(mktemp -d)
suffix="$$-$RANDOM-$RANDOM"
source_project="artci-backup-$suffix"
target_project="artci-recovery-$suffix"
env_file="$root/deploy/.env.example"
compose=(docker compose --env-file "$env_file" -f "$root/deploy/docker-compose.yml")
source_db=("${compose[@]}" --project-name "$source_project")
target_db=("${compose[@]}" --project-name "$target_project")
backup=(bash "$root/deploy/postgres-backup.sh")

cleanup() {
  "${source_db[@]}" down --volumes >&2 || true
  "${target_db[@]}" down --volumes >&2 || true
  rm -f -- "$scratch/identity" "$scratch/wrong-identity" "$scratch/artci.age" "$scratch/failed.age" "$scratch"/*.tmp.*
  rmdir -- "$scratch"
}
trap cleanup EXIT

"${source_db[@]}" up -d --wait postgres
"${target_db[@]}" up -d --wait postgres
target=$("${target_db[@]}" ps -q postgres)

{
  cat "$root/migrations/001_init.sql" "$root/migrations/002_merger.sql"
  cat <<'SQL'
INSERT INTO events (id, origin_time, location, magnitude, place_name, published_version)
VALUES ('11111111-1111-4111-8111-111111111111', '2025-08-10T16:53:00Z',
        ST_SetSRID(ST_MakePoint(28.17, 39.24), 4326)::geography, 4.2, 'Sındırgı', 1);
INSERT INTO reports (source, source_event_id, origin_time, location, magnitude, raw, event_id)
SELECT 'afad', 'backup-fixture', origin_time, location, magnitude, '{"sample":true}', id FROM events;
INSERT INTO subscriptions (chat_id, city, center, radius_km, min_mag)
VALUES (123456789, 'Balıkesir', ST_SetSRID(ST_MakePoint(27.89, 39.65), 4326)::geography, 100, 3.5);
INSERT INTO notifications (event_id, subscription_id, kind)
SELECT events.id, subscriptions.id, 'initial' FROM events CROSS JOIN subscriptions;
SQL
} | "${source_db[@]}" exec -T postgres psql -X --username=artci --dbname=artci -v ON_ERROR_STOP=1 -q

age-keygen -o "$scratch/identity"
age-keygen -o "$scratch/wrong-identity"
recipient=$(age-keygen -y "$scratch/identity")
archive="$scratch/artci.age"
export ARTCI_ENV_FILE="$env_file" ARTCI_COMPOSE_PROJECT="$source_project"

"${backup[@]}" backup "$recipient" "$archive"
[[ $(head -n 1 "$archive") == 'age-encryption.org/v1' ]]
[[ $(stat -c '%a' "$archive") == '600' ]]
checksum=$(sha256sum "$archive")
if "${backup[@]}" backup "$recipient" "$archive"; then
  printf 'Mevcut arşivin üstüne yazıldı.\n' >&2
  exit 1
fi
[[ $(sha256sum "$archive") == "$checksum" ]]
if ARTCI_COMPOSE_PROJECT="missing-$suffix" "${backup[@]}" backup "$recipient" "$scratch/failed.age"; then
  printf 'Yedek kaynak çalışmazken başarılı göründü.\n' >&2
  exit 1
fi
[[ ! -e $scratch/failed.age ]]
if compgen -G "$scratch/*.tmp.*" >/dev/null; then
  printf 'Yarım yedek dosyası kaldı.\n' >&2
  exit 1
fi

if "${backup[@]}" restore "$target" failed_restore "$scratch/wrong-identity" "$archive"; then
  printf 'Yanlış kimlikle geri yükleme başarılı oldu.\n' >&2
  exit 1
fi
[[ $(docker exec "$target" psql -XAt --username=artci --dbname=postgres -v ON_ERROR_STOP=1 \
  -c "SELECT count(*) FROM pg_database WHERE datname = 'failed_restore'") == '0' ]]

"${backup[@]}" restore "$target" artci_recovery "$scratch/identity" "$archive"
if "${backup[@]}" restore "$target" artci_recovery "$scratch/identity" "$archive"; then
  printf 'Mevcut veritabanına geri yüklemeye izin verildi.\n' >&2
  exit 1
fi
result=$(docker exec -i "$target" psql -XAt --username=artci --dbname=artci_recovery -v ON_ERROR_STOP=1 <<'SQL'
SELECT
  (SELECT count(*) FROM events WHERE magnitude = 4.2 AND ST_SRID(location::geometry) = 4326 AND published_version = 1) = 1
  AND (SELECT count(*) FROM reports r JOIN events e ON e.id = r.event_id
       WHERE r.source = 'afad' AND r.source_event_id = 'backup-fixture' AND r.raw = '{"sample":true}') = 1
  AND (SELECT count(*) FROM subscriptions WHERE chat_id = 123456789 AND radius_km = 100 AND min_mag = 3.5) = 1
  AND (SELECT count(*) FROM notifications n JOIN subscriptions s ON s.id = n.subscription_id
       JOIN events e ON e.id = n.event_id WHERE s.chat_id = 123456789 AND n.kind = 'initial') = 1;
SQL
)
[[ $result == 't' ]]
printf 'Şifreli arşivden olay, kaynak raporu, abonelik ve bildirim kaydı geri yüklendi.\n'
