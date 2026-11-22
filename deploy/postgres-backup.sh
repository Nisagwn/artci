#!/usr/bin/env bash
set -euo pipefail
umask 077

deploy_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)

usage() {
  printf '%s\n' \
    'Kullanım:' \
    '  postgres-backup.sh backup AGE_ALICISI YEDEK.age' \
    '  postgres-backup.sh restore HEDEF_KONTEYNER YENİ_VERİTABANI AGE_KİMLİĞİ YEDEK.age' >&2
  exit 2
}

require_tools() {
  for tool in docker age; do
    if ! command -v "$tool" >/dev/null; then
      printf 'Gerekli komut bulunamadı: %s\n' "$tool" >&2
      exit 1
    fi
  done
}

backup() (
  local recipient=$1 output=$2 temporary
  local compose=(docker compose --env-file "${ARTCI_ENV_FILE:-$deploy_dir/.env}" -f "$deploy_dir/docker-compose.yml")
  if [[ -n ${ARTCI_COMPOSE_PROJECT:-} ]]; then
    compose+=(--project-name "$ARTCI_COMPOSE_PROJECT")
  fi
  if [[ -e $output || -L $output ]]; then
    printf 'Yedek hedefi zaten var: %s\n' "$output" >&2
    return 1
  fi
  temporary=$(mktemp -- "${output}.tmp.XXXXXXXX")
  trap 'rm -f -- "$temporary"' EXIT
  "${compose[@]}" exec -T postgres sh -ec '
    exec pg_dump --username="$POSTGRES_USER" --dbname="$POSTGRES_DB" --format=custom --no-owner --no-acl
  ' | age --encrypt --recipient "$recipient" > "$temporary"
  # Aynı dizinde atomik olarak yayımla. Var olan dosya veya dizinin üstüne yazma.
  ln -T -- "$temporary" "$output"
  printf 'Şifreli yedek hazır: %s\n' "$output"
)

restore() (
  local container=$1 database=$2 identity=$3 archive=$4
  if [[ ! $database =~ ^[a-z_][a-z0-9_]{0,62}$ ]]; then
    printf 'Geçersiz hedef veritabanı adı: %s\n' "$database" >&2
    return 2
  fi
  if [[ ! -f $identity || ! -r $identity || ! -f $archive || ! -r $archive ]]; then
    printf 'Age kimliği ve yedek okunabilir dosyalar olmalı.\n' >&2
    return 2
  fi

  # CREATE DATABASE mevcut hedefi reddeder. template0, PostGIS dahil arşivdeki
  # şemanın boş bir veritabanına kurulmasını sağlar.
  docker exec -- "$container" sh -ec '
    exec createdb --username="$POSTGRES_USER" --template=template0 -- "$1"
  ' sh "$database"

  cleanup_failed_restore() {
    if ! docker exec -- "$container" sh -ec '
      exec dropdb --username="$POSTGRES_USER" --if-exists -- "$1"
    ' sh "$database"; then
      printf 'Başarısız geri yüklemenin hedefi kaldı: %s/%s\n' "$container" "$database" >&2
    fi
  }
  trap cleanup_failed_restore EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM

  age --decrypt --identity "$identity" < "$archive" | docker exec -i -- "$container" sh -ec '
    exec pg_restore --username="$POSTGRES_USER" --dbname="$1" --single-transaction --no-owner --no-acl
  ' sh "$database"
  trap - EXIT INT TERM
  printf 'Geri yükleme tamamlandı: %s/%s\n' "$container" "$database"
)

case ${1:-} in
  backup) [[ $# == 3 ]] || usage; require_tools; backup "$2" "$3" ;;
  restore) [[ $# == 5 ]] || usage; require_tools; restore "$2" "$3" "$4" "$5" ;;
  *) usage ;;
esac
