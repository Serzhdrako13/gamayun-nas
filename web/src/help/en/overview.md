# Overview

GamayunNas is network-attached storage: several disks in one box that computers and phones at home
or in the office reach over the network as a shared folder. You manage it from a browser, on these pages.
Think of a house: the disks are the foundation, the pool is the house itself, shared folders are the
rooms, and users are residents with keys to the rooms they need.

## What you see
- **Home** — pool and disk state and running tasks. If something is wrong with a pool, a banner or a
  line with an **Open** button appears here.
- Sections: **Storage**, **Shared folders**, **Files**, **Users**, **Log**, **Notifications**,
  **Settings**, **Help**. In the “Tabs” style they are tiles on the home page; in the “Desktop” style —
  **▦ Menu** and shortcuts. Change the style in **Settings → Appearance**.
- At the top — **Search Ctrl+K** (jump to a section or a pool), theme and language switches,
  the power button and sign-out.
- The **?** button next to a block opens help about that block. No button means that help section
  has not been written yet.

## How it fits together
1. **Disks** are combined into a **pool** — one storage space for data. How exactly is set by the layout:
   [Pools and RAID levels](help:storage.pools.raid).
2. **Shared folders** are created on the pool — computers see them over the network (SMB).
3. **Users** and groups get permissions on shared folders.
4. The system watches disks and pools by itself. What happened is written to the **Log** (events) and to
   **Notifications**; notifications can also be sent by email or webhook.

## Help
- The help window opens on top of the page: whatever you typed on the page is kept.
- Drag the window by its title, resize it from the bottom-right corner, minimize it to a bar at the
  bottom (**–**) or maximize it (**□**). The **⧉** button opens the section as a page.
- **← Back** returns to the previous section; the **Search help** field searches all sections.
- The contents with search is the **Help** section.
- On a phone, help is a panel at the bottom of the screen.

## If something is wrong
- [Pool degraded (disk dropped out)](help:storage.pools.incomplete) — a pool disk failed or disconnected.
- [Read-only pool](help:storage.pools.readonly) — the system blocked writing so as not to lose data.

RAID protects against a disk failure, not against deleted files: keep a backup of important data on
another device.

:::details For administrators
Service settings — `/etc/gamayun-nas/gamayund.json` and `/etc/gamayun-nas/gamayun-helper.json`;
database and certificates — `/var/lib/gamayun-nas`. Service logs: `journalctl -u gamayund`,
`journalctl -u gamayun-helper`. A pool is built in layers: md (RAID) → dm-integrity (if integrity
protection is on) → LVM → Btrfs.
:::
