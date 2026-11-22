#!/bin/bash
# Назначение: сборка gamayun-nas-apps_<версия>_amd64.deb: Docker-стек закреплённых версий
#   из packaging/apps/versions.lock + юниты gamayun-docker/gamayun-containerd.
# Запуск:  scripts/build-deb-apps.sh   → dist/gamayun-nas-apps_<версия>_amd64.deb (путь — последней строкой)
# Параметры: OUT — каталог результата (dist/); VERSION — явная версия (должна совпасть с gamayun-nas, Depends «=»).
# Версия по умолчанию — как у scripts/build-deb.sh для рабочего дерева.
# Идемпотентно: перезаписывает тот же файл; бинарники — из кеша dist/vendor (scripts/fetch-apps-vendor.sh).
# Откат: удалить файл из dist/.
set -euo pipefail
cd "$(dirname "$0")/.."
ROOT=$PWD; OUT=${OUT:-$ROOT/dist}
V="0.1.0~alpha.$(git rev-list --count HEAD)+g$(git rev-parse --short HEAD)"
[ -z "$(git status --porcelain --untracked-files=no)" ] || V="$V.dirty"
[ -z "${VERSION:-}" ] || V=$VERSION
VEN=$(scripts/fetch-apps-vendor.sh | tail -1)
ver() { awk -v n="$1" '$1==n {print $2}' packaging/apps/versions.lock; }
DV=$(ver docker-static); CV=$(ver compose); YV=$(ver caddy)
T=$(mktemp -d); trap 'rm -rf "$T"' EXIT
P=$T/pkg; B=$P/usr/lib/gamayun-nas-apps/bin
mkdir -p $P/DEBIAN $B $P/usr/lib/systemd/system $P/usr/libexec/docker/cli-plugins $P/usr/share/doc/gamayun-nas-apps
tar xzf "$VEN/docker-static-$DV" -C $T
install -m 755 $T/docker/* $B/
install -m 755 "$VEN/compose-$CV" $B/docker-compose
mkdir -p $T/caddy && tar xzf "$VEN/caddy-$YV" -C $T/caddy caddy LICENSE && install -m 755 $T/caddy/caddy $B/caddy
install -m 644 packaging/apps/caddy-bootstrap.json $P/usr/lib/gamayun-nas-apps/
mkdir -p $P/usr/bin; ln -s /usr/lib/gamayun-nas-apps/bin/docker $P/usr/bin/docker
ln -s /usr/lib/gamayun-nas-apps/bin/docker-compose $P/usr/libexec/docker/cli-plugins/docker-compose
install -m 644 packaging/apps/systemd/*.service $P/usr/lib/systemd/system/
install -m 644 packaging/apps/versions.lock $P/usr/share/doc/gamayun-nas-apps/
install -m 644 packaging/apps/README.md packaging/apps/deb/copyright packaging/licenses/Apache-2.0.txt $P/usr/share/doc/gamayun-nas-apps/
install -m 644 $T/caddy/LICENSE $P/usr/share/doc/gamayun-nas-apps/LICENSE.caddy
install -m 755 packaging/apps/deb/postinst packaging/apps/deb/prerm packaging/apps/deb/postrm $P/DEBIAN/
SIZE=$(du -sk --exclude=DEBIAN $P | cut -f1)
sed -e "s/@VERSION@/$V/g" -e "s/@SIZE@/$SIZE/" -e "s/@DOCKER@/$DV/" -e "s/@COMPOSE@/$CV/" -e "s/@CADDY@/$YV/" packaging/apps/deb/control.in >$P/DEBIAN/control
(cd $P && find usr -type f | sort | xargs md5sum >DEBIAN/md5sums)
TO="--owner=0 --group=0 --numeric-owner --sort=name --mtime=@$(git log -1 --format=%ct HEAD)"
tar $TO -C $P/DEBIAN -czf $T/control.tar.gz ./control ./md5sums ./postinst ./prerm ./postrm
tar $TO -C $P -czf $T/data.tar.gz ./usr
echo 2.0 >$T/debian-binary
mkdir -p "$OUT"; F="$OUT/gamayun-nas-apps_${V}_amd64.deb"; rm -f "$F"
(cd $T && ar rcD "$F" debian-binary control.tar.gz data.tar.gz)
echo "$F"
