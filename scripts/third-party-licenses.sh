#!/bin/bash
# Назначение: собрать тексты лицензий сторонних компонентов, вошедших в бинарники gamayund/gamayun-helper
#   (Go-модули) и в веб-интерфейс (Svelte), в packaging/licenses/THIRD-PARTY.txt.
# Запуск: scripts/third-party-licenses.sh (из корня; нужен go-кэш модулей и web/node_modules). Идемпотентно.
# Откат: git checkout -- packaging/licenses/THIRD-PARTY.txt
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
O=packaging/licenses/THIRD-PARTY.txt
{
  echo "Сторонние компоненты GamayunNas и их лицензии / Third-party components of GamayunNas and their licenses"
  echo
  for m in $(go list -deps -f '{{with .Module}}{{if not .Main}}{{.Path}}@{{.Version}}{{end}}{{end}}' ./cmd/gamayund ./cmd/gamayun-helper | sort -u); do
    d=$(go list -m -f '{{.Dir}}' "${m%@*}"); l=$(ls "$d" | grep -i -m1 -E '^(licen[cs]e|copying)(\.(md|txt))?$')
    printf '==== %s ====\n\n' "$m"; cat "$d/$l"; echo
  done
  for p in svelte; do
    v=$(node -p "require('./web/node_modules/$p/package.json').version")
    printf '==== %s@%s (npm, web interface) ====\n\n' "$p" "$v"; cat web/node_modules/$p/LICENSE*; echo
  done
} >"$O"
echo "$O: $(grep -c '^==== ' "$O") компонентов"
