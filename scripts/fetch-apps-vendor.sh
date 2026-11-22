#!/bin/bash
# Назначение: загрузка закреплённых компонентов Docker-стека (packaging/apps/versions.lock) в dist/vendor/ со сверкой sha256.
# Запуск: scripts/fetch-apps-vendor.sh   → печатает каталог кеша последней строкой.
# Параметры: VENDOR — каталог кеша (по умолчанию dist/vendor).
# Идемпотентно: файл с верной суммой не перекачивается; при несовпадении — ошибка, файл не принимается.
# Откат: удалить dist/vendor/.
set -euo pipefail
cd "$(dirname "$0")/.."
V=${VENDOR:-$PWD/dist/vendor}; mkdir -p "$V"
grep -vE '^\s*(#|$)' packaging/apps/versions.lock | while read -r name ver url sum; do
  f="$V/$name-$ver"
  if [ -f "$f" ] && echo "$sum  $f" | sha256sum -c --quiet - >/dev/null 2>&1; then continue; fi
  curl -sfL --retry 3 -o "$f.part" "$url"
  if ! echo "$sum  $f.part" | sha256sum -c --quiet - >/dev/null 2>&1; then mv "$f.part" "$f.bad"; echo "ОШИБКА: sha256 $name $ver (файл $f.bad)" >&2; exit 1; fi
  mv "$f.part" "$f"
done
echo "$V"
