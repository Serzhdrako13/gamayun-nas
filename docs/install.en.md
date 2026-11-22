# Installing GamayunNas

[Русский](install.md) · [README](../README.en.md)

This guide takes you from a bare machine to a working NAS: preparing Debian, installing the packages,
the first-run wizard, the first pool and network access. Upgrades, removal, moving pools and
troubleshooting are covered at the end.

## Contents
1. [Requirements](#1-requirements)
2. [Preparing Debian](#2-preparing-debian)
3. [Installing the packages](#3-installing-the-packages)
4. [First sign-in and the wizard](#4-first-sign-in-and-the-wizard)
5. [SSH access after the wizard](#5-ssh-access-after-the-wizard)
6. [First pool and shared folders](#6-first-pool-and-shared-folders)
7. [Docker applications](#7-docker-applications)
8. [Upgrading](#8-upgrading)
9. [Removal](#9-removal)
10. [Reinstalling the OS and moving pools](#10-reinstalling-the-os-and-moving-pools)
11. [Troubleshooting](#11-troubleshooting)

## 1. Requirements

| | Minimum | Recommended |
|---|---|---|
| OS | Debian 13 (trixie), amd64 | Debian 13 without a desktop environment |
| CPU | 2 x86-64 cores | 4 cores |
| Memory | 2 GB | 4 GB or more (8 GB with Docker applications) |
| System disk | 16 GB, separate from the data disks | SSD, 32 GB or more |
| Data disks | 1 | 2 or more for RAID; equal sizes are not required (SHR) |
| Network | Ethernet, fixed address (DHCP reservation or static) | 1 Gbit/s or faster |

> **Data disks are fully repartitioned** when a pool is created. GamayunNas does not touch disks
> that have partitions or data and does not offer them for a new pool, but still make sure the
> selected disks hold nothing you need.

Ports: **8780/tcp** — web interface (HTTPS), **445/tcp** — SMB, **22/tcp** — SSH (when enabled),
**443/tcp** — app web addresses (when published), **5353/udp** — mDNS (`*.local` names).

## 2. Preparing Debian

1. Install Debian 13 on the **system disk** from the regular installer image (netinst).
   In software selection keep only **SSH server** and **standard system utilities**.
   Do not partition the data disks.
2. Sign in and update the system:
   ```sh
   sudo apt update && sudo apt full-upgrade -y
   ```
3. Give the server a fixed address (a reservation on your router or a static address in
   `/etc/network/interfaces`).
4. Check that the data disks are visible and empty:
   ```sh
   lsblk -o NAME,SIZE,TYPE,MODEL,SERIAL,MOUNTPOINTS
   ```

## 3. Installing the packages

Download the packages from the repository **Releases** page:

- `gamayun-nas_<version>_amd64.deb` — the NAS itself (required);
- `gamayun-nas-apps_<version>_amd64.deb` — Docker applications (optional; conflicts with `docker-ce`
  and `docker.io` from other sources);
- `SHA256SUMS` — checksums.

```sh
sha256sum -c SHA256SUMS --ignore-missing
sudo apt install ./gamayun-nas_*_amd64.deb
# optional:
sudo apt install ./gamayun-nas-apps_*_amd64.deb
```

`apt` installs the dependencies (Samba, mdadm, LVM, Btrfs, smartmontools, fail2ban and others).
After installation the `gamayund`, `gamayun-helper` and `gamayun-integrity` services are running:

```sh
systemctl status gamayund gamayun-helper --no-pager
```

> The package extends `/etc/samba/smb.conf` (the original is kept as `/etc/samba/smb.conf.pre-gamayun`)
> and diverts `/etc/logrotate.d/fail2ban` so that log rotation does not fail while SSH protection is
> off. The stock file is restored when the package is removed.

## 4. First sign-in and the wizard

1. Get the first sign-in token:
   ```sh
   sudo cat /var/lib/gamayun-nas/setup-token
   ```
2. Open `https://<server-address>:8780/` in a browser. The certificate is self-signed, so the browser
   will warn you; proceed anyway. You can upload your own certificate later:
   **Settings → HTTPS certificate**.
3. Enter the token and choose the administrator name and password (at least 10 characters).
4. The wizard offers to:
   - **restore settings from a backup** — when you are moving a NAS (see [section 10](#10-reinstalling-the-os-and-moving-pools));
   - set the **server name** and **time zone**;
   - create the **first pool** or attach one **found** on the disks.
5. After the wizard the home page opens. The token is no longer valid.

Forgot the administrator password? Change it from the server console:

```sh
sudo -u gamayun /usr/lib/gamayun-nas/gamayund admin passwd -name <name>
```

## 5. SSH access after the wizard

After the wizard **the SSH service is off** by default, and sign-in as `root` or system accounts
(such as `debian`) is closed. Sessions that are already open are not dropped.

If you manage the server over SSH:

1. **Users** — create a user (or open an existing one) and enable **SSH sign-in**. NAS administrators
   have it on by default, and only they get `sudo`.
2. **Settings → SSH service → Enable.**
3. Sign in as that user with their NAS password:
   ```sh
   ssh <user>@<server-address>
   ```

Password brute-force protection (fail2ban) is configured on the same page.

<details>
<summary>For administrators: key-based root login</summary>

On test servers you can keep key-based login for selected system accounts (passwords stay
disabled for them). Add them to `/etc/gamayun-nas/gamayun-helper.json`:

```json
{ "mount_root": "/srv/gamayun-nas", "destructive_allow_ids": [], "ssh_dev_access": ["root"] }
```

and restart the helper: `sudo systemctl restart gamayun-helper`. Do this **before** finishing the
wizard if you manage the server over SSH as root.

</details>

## 6. First pool and shared folders

1. **Storage → Pools → Create pool.** Choose disks and a layout. For two or more disks of different
   sizes use **SHR** (survives one disk failure) or **SHR-2** (two). **Data integrity** is on by
   default — keep it.
2. Review the plan: which disks will be partitioned and how much space you get. Confirm by typing
   the pool name.
3. The pool is writable right away; the initial RAID sync runs in the background and is shown on the
   pool card.
4. **Shared folders → Create:** name, pool, user and group permissions.
5. Connecting over the network:
   - Windows: `\\<server-name>\<folder>` or `\\<address>\<folder>`;
   - macOS: Finder → **Connect to Server** → `smb://<address>/<folder>`;
   - Linux: `smb://<address>/<folder>` in the file manager.
   Sign in with a NAS user name and password.
6. **Shared folders → Snapshots** — snapshot schedule; restore from the folder card or via
   **Previous Versions** in Windows.

Pools are mounted at `/srv/gamayun-nas/<pool>`.

## 7. Docker applications

> **In development.** Works, but the interface and formats may change between alpha versions.

Requires the `gamayun-nas-apps` package.

1. **Apps** — choose a pool and enable applications. Docker keeps images and data there (`<pool>/.apps`).
2. **Add app** — paste a docker compose file. The system shows the risks (privileges, ports, folder
   access) and asks for confirmation.
3. **Web address** on the app card publishes it as `https://<app>-<nas-name>.local/`. For browsers to
   trust the address, install the **NAS root certificate** (link on the same card) on your devices.
   Port 443 must be free.

## 8. Upgrading

Download the new packages and install them **in one command** (if both are installed):

```sh
sudo apt install ./gamayun-nas_*_amd64.deb ./gamayun-nas-apps_*_amd64.deb
```

Services restart automatically; settings and pools are kept. Saving a configuration backup first is
recommended: **Settings → Configuration backup → Save to pools now**.

The alpha is upgraded by package only, with no automatic rollback. To go back, install the previous
version's package the same way.

## 9. Removal

```sh
sudo apt remove gamayun-nas-apps gamayun-nas     # services removed, settings and pools kept
sudo apt purge gamayun-nas-apps gamayun-nas      # also removes the configuration in /etc/gamayun-nas
```

Neither `remove` nor `purge` deletes the settings database (`/var/lib/gamayun-nas`), the pool
directory (`/srv/gamayun-nas`) or data on the disks. To erase everything, delete the pools in the web
interface before removing the package.

## 10. Reinstalling the OS and moving pools

Pools store a description of their layout and configuration backups on the disks themselves. After
reinstalling Debian or moving the disks to another machine:

1. Install Debian 13 and the GamayunNas package ([sections 2–3](#2-preparing-debian)). Connect all
   pool disks.
2. The wizard (or **Storage → Pools**) shows a **Found pools** card. Type the pool name and click
   **Import** — type, RAID levels and data integrity are read from the disks.
3. If some disks are missing, the pool is attached only after an extra checkbox and runs degraded;
   a returning disk rejoins the array automatically.
4. The wizard offers to **restore settings** from the backup on the pool: users, groups, shared
   folders, permissions, schedules. User passwords are set anew.

Configuration backups are saved to every pool automatically (hourly, when settings have changed);
they can also be kept on an SFTP server, written to a USB drive or downloaded as a file
(**Settings → Configuration backup**).

## 11. Troubleshooting

| What to check | Command |
|---|---|
| Service status | `systemctl status gamayund gamayun-helper gamayun-integrity` |
| Daemon log | `journalctl -u gamayund -b` |
| Disk operations log | `journalctl -u gamayun-helper -b` |
| RAID arrays | `cat /proc/mdstat` |
| Volumes and pools | `lsblk`, `sudo lvs`, `findmnt -t btrfs` |
| Configuration | `/etc/gamayun-nas/gamayund.json`, `/etc/gamayun-nas/gamayun-helper.json` |

Common situations:

- **The page does not open.** Check that `gamayund` runs and listens: `sudo ss -tlnp | grep 8780`.
  Use `https://`.
- **Sign-in is locked after wrong passwords.** Wait for the lockout to expire or run
  `sudo systemctl restart gamayund`.
- **Pool is read-only.** Open the pool card: it shows the reason (missing disk, write error) and
  what to do. The built-in help (the **?** button) explains it in detail.
- **A disk is not offered for a pool.** It has partitions or file system signatures. If you do not
  need its data, wipe it manually (`sudo wipefs -a /dev/sdX` — **irreversible**).

Server messages (errors, events) are currently in Russian; the interface and help are in English.
