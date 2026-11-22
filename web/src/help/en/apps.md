# Apps

Apps are additional programs that run right on the NAS: a media server, a photo library, file sync.
Each one lives in its own **container** — a separate "box" with its own program files — so apps do not
interfere with each other or with the NAS itself. Containers are run by Docker.

## What you see
- The **Apps** switch and pool selection. The pool is chosen once: it holds the programs, app data and
  their logs. Moving apps to another pool is not yet possible in the web interface.
- The state badge: **running**, **starting**, **unavailable**, **paused**, **error**, **not installed**.
  What to do in each case is described in
  [Apps unavailable or paused](help:apps.unavailable).
- The **Installed apps** list with the **Add app** button — see
  [Add app](help:apps.add).
- The **Ports** tab — which NAS network ports are used by what ([Ports](help:apps.ports)).
- The **Catalog** tab — verified apps installed in a couple of steps ([App catalog](help:apps.catalog)).

## How it works
1. Once enabled, Docker starts only when the selected pool is connected and writable. No pool — Docker
   does not start at all, so app data does not end up on the system disk.
2. If the pool disappears or becomes read-only, Docker stops along with all apps.
3. When Docker is running again, the apps that should be running start by themselves.

## What not to do
- Do not start containers manually over SSH: this section does not manage them and only shows their
  count ("Containers started outside this section: …").

See also: [App states](help:apps.states), [Risks](help:apps.risks).

:::details For administrators
The app subsystem is installed as a separate package, `gamayun-nas-apps`. On the pool, apps use the
hidden folder `.apps`: `.apps/docker` (images and containers), `.apps/appdata/<id>` (app data),
`.apps/projects/<id>` (app files). The selected pool is stored in `/etc/gamayun-nas/apps.json`.
Service log: `journalctl -u gamayund`.
:::
