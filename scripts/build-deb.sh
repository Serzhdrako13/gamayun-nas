#!/bin/bash
# Назначение: сборка пакета gamayun-nas_<версия>_amd64.deb (alpha, без A/B). Без dpkg-deb: ar + tar.
# Запуск:  scripts/build-deb.sh [REF]          → dist/gamayun-nas_<версия>_amd64.deb (путь печатается последней строкой)
# Параметры:
#   REF  — коммит/ветка для сборки (по умолчанию рабочее дерево как есть). Для REF берётся git worktree
#          во временном каталоге; packaging/deb (сценарии пакета) — всегда из текущего дерева.
#   OUT  — каталог результата (по умолчанию dist/)
#   VERSION — явная версия (только для рабочего дерева; проверка обновления)
# Версия: 0.1.0~alpha.<число коммитов>+g<hash>[.dirty] — растёт с каждым коммитом main, dpkg сравнивает верно.
# Идемпотентно: повторная сборка перезаписывает тот же файл. Откат: удалить файл из dist/.
set -euo pipefail
cd "$(dirname "$0")/.."
ROOT=$PWD; REF=${1:-}; OUT=${OUT:-$ROOT/dist}
T=$(mktemp -d); trap 'cd "$ROOT"; [ -n "$REF" ] && git worktree remove --force "$T/src" 2>/dev/null; rm -rf "$T"' EXIT
if [ -n "$REF" ]; then
  git worktree add --detach -q "$T/src" "$REF"; SRC=$T/src
  V="0.1.0~alpha.$(git rev-list --count "$REF")+g$(git rev-parse --short "$REF")"
else
  SRC=$ROOT
  V="0.1.0~alpha.$(git rev-list --count HEAD)+g$(git rev-parse --short HEAD)"
  [ -z "$(git status --porcelain --untracked-files=no)" ] || V="$V.dirty"
  [ -z "${VERSION:-}" ] || V=$VERSION
fi
P=$T/pkg; mkdir -p $P/DEBIAN $P/usr/lib/gamayun-nas $P/usr/lib/systemd/system $P/usr/lib/sysusers.d $P/usr/share/gamayun-nas $P/usr/share/doc/gamayun-nas
(cd "$SRC" && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath \
  -ldflags "-s -w -X gamayun-nas/internal/version.Version=$V" -o $P/usr/lib/gamayun-nas/ ./cmd/gamayund ./cmd/gamayun-helper)
install -m 644 "$SRC"/packaging/systemd/*.service $P/usr/lib/systemd/system/
install -m 644 "$SRC"/packaging/systemd/gamayun-nas.sysusers.conf $P/usr/lib/sysusers.d/gamayun-nas.conf
install -m 644 "$SRC"/packaging/config/*.json $P/usr/share/gamayun-nas/
install -m 644 "$SRC"/README.md "$SRC"/LICENSE "$SRC"/packaging/deb/copyright "$SRC"/packaging/licenses/THIRD-PARTY.txt $P/usr/share/doc/gamayun-nas/
install -m 755 packaging/deb/postinst packaging/deb/prerm packaging/deb/postrm $P/DEBIAN/
SIZE=$(du -sk --exclude=DEBIAN $P | cut -f1)
sed -e "s/@VERSION@/$V/" -e "s/@SIZE@/$SIZE/" packaging/deb/control.in >$P/DEBIAN/control
(cd $P && find usr -type f | sort | xargs md5sum >DEBIAN/md5sums)
TO="--owner=0 --group=0 --numeric-owner --sort=name --mtime=@$(git log -1 --format=%ct ${REF:-HEAD})"
tar $TO -C $P/DEBIAN -czf $T/control.tar.gz ./control ./md5sums ./postinst ./prerm ./postrm
tar $TO -C $P -czf $T/data.tar.gz ./usr
echo 2.0 >$T/debian-binary
mkdir -p "$OUT"; F="$OUT/gamayun-nas_${V}_amd64.deb"; rm -f "$F"
(cd $T && ar rcD "$F" debian-binary control.tar.gz data.tar.gz)
echo "$F"
