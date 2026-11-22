# Configuration backup

A configuration backup holds everything you set up in the interface: users, groups, shared folders and
permissions, server and notification settings. If the system disk fails, you can reinstall the NAS and
bring the settings back from a backup in a couple of minutes instead of recalling them from memory.

## What you see
- **Download backup** — save the backup file to your computer.
- **Backups in pools** and **Save to pools now**.
- **External drive (USB)** — **Find drives** and **Write**. FAT32, exFAT and ext4 are supported;
  the drive must not be attached as a pool.
- **Network storage (SFTP)** — upload to another server. Sign-in uses the NAS key; no password is
  stored: add the NAS public key to the user on the server, then **Check fingerprint** and compare it
  with the server. **Upload automatically** — together with backups to pools; **Upload now** — manually.

## How it works
- A backup is made automatically when settings change (checked hourly) and at least once a day — to
  every pool and, if enabled, to SFTP. The latest 30 backups are kept.
- Passwords and secrets are not included — they are set again on restore.
- On a new installation — the **Restore from backup** button in the initial setup wizard, including
  from a backup on an attached pool.

## What not to do
- Do not confirm the SFTP server key if the fingerprint does not match what the server shows.

See also: [Settings](help:settings), [Found pools](help:storage.pools.found).

:::details For administrators
On an already configured system, restore is run from the server console:
```
sudo systemctl stop gamayund
sudo -u gamayun gamayund config plan -file backup.json.gz
sudo -u gamayun gamayund config restore -file backup.json.gz
sudo systemctl start gamayund
```
:::
