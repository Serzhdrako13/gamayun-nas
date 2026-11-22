# Shared folders

A shared folder is a folder on a pool that is visible over the network: in Windows as a network drive,
in macOS and Linux as a network share (SMB). Like a room in a house: each resident gets their own key —
some may only look, others may also move the furniture.

## What you see
- On the left — **top-level** shared folders: name, pool (**pool …**), permission summary
  (**write: …, read: …**). Folder buttons: **Permissions**, **Snapshots**, **Remove**.
- **Create folder** — name, **Description** and pool. No pools yet — create one in
  [Storage](help:storage) first.
- On the right — **Subfolders** of the selected top-level folder: a tree where you can create, rename,
  move and delete folders. Files and links are not shown here; only an empty folder can be deleted.
  To work with files, use [Files](help:files).
- The mark **incompatible with Windows · rename** — a name Windows will not show or will garble.
- **Apply Samba configuration** — rewrite the network access settings if they got out of sync.

## Permissions
- Permissions are set on the top-level folder and apply to everything inside. Levels: **None**,
  **Read**, **Write** (read and write).
- Permissions are granted to users and groups. An explicit **None** for a user overrides access
  granted through a group.
- The same permissions apply in the web **Files** section.

## Snapshots
A snapshot is the state of a folder at a point in time, like a photo of a shelf: if something was
deleted or encrypted by ransomware, you can take the earlier version from the snapshot.
- **Scheduled snapshots**: how many of the latest hourly, daily, weekly and monthly snapshots to keep —
  one per period. Snapshots are taken at the step of the most frequent enabled level.
- **Create snapshot now** — a manual snapshot; manual snapshots are never deleted automatically.
- If the pool has less than 10 % free, scheduled snapshots are skipped; on a read-only pool no new
  snapshots are created.
- Snapshots are read-only. In Windows, earlier versions appear in the file or folder properties →
  **Previous Versions**.
- **Restore** creates a copy next to the original location, with the snapshot time in the name —
  current files are not overwritten.

## What not to do
- **Remove** takes the folder off the network, but the directory and snapshots stay on the pool.
  It does not free space.
- Snapshots live on the same pool: if the pool is lost, they are lost too. They do not replace a
  backup on another device.

See also: [Files](help:files), [Users](help:users), [Storage](help:storage).
