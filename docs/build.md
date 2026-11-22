# Сборка из исходников

[English](build.en.md) · [README](../README.md)

## Что нужно
- Linux amd64, `git`, `bash`, `binutils` (`ar`), `curl`, `sha256sum`;
- **Go 1.24** или новее;
- **Node.js 22** или новее и `npm` — только если меняете веб-интерфейс.

## Структура
| Каталог | Что внутри |
|---|---|
| `cmd/gamayund` | демон управления: API, веб, задачи, события |
| `cmd/gamayun-helper` | исполнитель операций от root (диски, Samba, SSH) |
| `cmd/gamayun-catalog` | инструмент подписи каталога приложений |
| `cmd/gamayun-disks` | диагностический снимок дисков (JSON) |
| `internal/` | логика: хранилище, NAS, приложения, уведомления и т. д. |
| `web/` | интерфейс на Svelte 5; сборка попадает в `internal/web/dist` и встраивается в бинарник |
| `packaging/` | юниты systemd, конфигурация по умолчанию, скрипты пакетов |
| `api/openapi.yaml` | описание HTTP API |

Комментарии в коде и сообщения сервера — на русском; интерфейс и справка есть на английском.

## Тесты
```sh
go vet ./... && go test ./...
cd web && npm ci && npm test
```

## Веб-интерфейс
Собранный интерфейс уже лежит в `internal/web/dist`. После изменений в `web/`:
```sh
cd web && npm ci && npm run build
```

## Пакеты .deb
```sh
scripts/build-deb.sh                 # gamayun-nas_<версия>_amd64.deb в dist/
scripts/fetch-apps-vendor.sh         # Docker, Compose, Caddy закреплённых версий (сверка sha256)
scripts/build-deb-apps.sh            # gamayun-nas-apps_<версия>_amd64.deb в dist/
```

Версия берётся из git: `0.1.0~alpha.<число коммитов>+g<коммит>`; при незакоммиченных изменениях
добавляется `.dirty`. Каталог результата меняется переменной `OUT=…`.

Версии Docker-стека закреплены в `packaging/apps/versions.lock` (адрес и sha256 каждого архива).

## Каталог приложений
```sh
go build -o gamayun-catalog ./cmd/gamayun-catalog
./gamayun-catalog -h                  # команды: keygen, build, sign, verify, pack
```
Открытый ключ каталога кладётся в каталог из `catalog.keys_dir` конфигурации `gamayund`,
адрес архива — в `catalog.url` (`/etc/gamayun-nas/gamayund.json`).
