# gamayun-nas-apps

Docker applications for GamayunNas / Приложения Docker для GamayunNas.

This package ships pinned, unmodified static builds of Docker Engine, containerd, runc,
Docker Compose and Caddy (versions: `versions.lock`). They run only under the
`gamayun-docker`, `gamayun-containerd` and `gamayun-caddy` units, which are started by
`gamayund` once applications are enabled on a pool in the web interface
(**Apps → Enable**). Docker data lives on that pool (`<pool>/.apps`).

Пакет содержит закреплённые немодифицированные сборки Docker Engine, containerd, runc,
Docker Compose и Caddy (версии — `versions.lock`). Они работают только как службы
`gamayun-docker`, `gamayun-containerd` и `gamayun-caddy`, которые запускает `gamayund`
после включения приложений на пуле в веб-интерфейсе (**Приложения → Включить**).
Данные Docker хранятся на этом пуле (`<пул>/.apps`).

The package conflicts with `docker-ce`, `docker.io`, `containerd` and `runc` from other sources.
Пакет несовместим с `docker-ce`, `docker.io`, `containerd` и `runc` из других источников.
