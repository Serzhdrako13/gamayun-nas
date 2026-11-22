# Add app

An app is added from a **compose** file — a short YAML description: which program (image) to download,
which ports to open and where to keep data. Such files are usually published by the program authors.
An app added this way is marked **Custom**: GamayunNas has not verified it, and it runs "as is".

## What you see
- **Identifier** — a short Latin name (`jellyfin`, `my-app`). It cannot be changed later.
- **Name** — how the app is labelled in the list.
- **Compose file (YAML)** — the file text; **Load file** — take it from your computer.
- **Variables (.env)** — `NAME=value` lines, for example passwords. In compose they replace `${NAME}`.
  After saving, the values are no longer shown — only the names.
- **Check** — parse the file without starting anything. Below appear: what cannot be started, port
  conflicts, ports marked **available on the network**, risks to confirm, and the list
  **Changed by the check**.
- **Create** becomes available when there are no blockers or conflicts and all risks are confirmed.

## How it works
1. Before starting, the file is adjusted to the NAS rules. What changes is shown in
   **Changed by the check**:
   - ports are opened only on the NAS itself (address 127.0.0.1) until you check
     **available on the network**;
   - `restart: always` and `unless-stopped` are replaced with "restart on failure, up to 5 times" —
     otherwise an app crashing in a loop would look like it is running;
   - the container log is limited to 3 files of 10 MB;
   - `container_name` is removed — the NAS names the containers.
2. **App data** — a relative path in a volume: `./data:/config` goes to a separate folder of this app on
   the pool. Missing folders are created automatically.
3. **Shared folder** — by its full path, shown by the check ("Shared folders: …"). After the check,
   **Shared folder access** appears: for each folder — **read only** (default) or **read and write**.
   The app runs under its own account and sees only these folders — see
   [Account and folder access](help:apps.access).
4. Clicking **Create** downloads the image and starts the app. If the start fails, everything created is
   removed — the system returns to its original state, and the task shows the reason.

## What not to do
- Do not use system folders in volumes (`/etc`, `/`, `/var/lib/docker`) — the check will reject them.
- Do not put passwords directly into the compose text — use **Variables (.env)**.

See also: [Account and folder access](help:apps.access), [Ports](help:apps.ports), [App states](help:apps.states).

:::details For administrators
Not supported: `build`, `include`, `extends`, `env_file` (except `.env`), `configs`/`secrets`, external
networks and volumes, port ranges, publish addresses other than 127.0.0.1 and 0.0.0.0. At most
20 services and 256 KiB per file. App data: `<pool>/.apps/appdata/<id>`.
:::
