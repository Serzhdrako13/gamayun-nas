# Storage

Storage is where the NAS keeps your data: the physical **disks** and the **pools** built from them.
A pool is several disks combined into one storage space: from the outside it is a single place for
shared folders, while inside the data is spread across the disks so that it survives the failure of
one or two of them. Think of a closet with several shelves: things sit on different shelves, but you
see one closet.

## What you see
The **Storage** window has two tabs:
- **Pools** — the list of pools: name, layout, number of disks, the “integrity protection” mark and the
  state. The **Create pool** button starts the wizard. Click a pool to open its card.
- **Disks** — all NAS disks: free, system and already used in pools, with the SMART result.
  Details — [Disks](help:storage.disks).
- **Found pools** — appear when the attached disks hold a pool this system does not know yet (for
  example, after a reinstall). See [Found pools](help:storage.pools.found).

## Pool state
| State | Meaning |
|---|---|
| **Healthy** | everything is fine |
| **Syncing** | the pool has just been created and the disks are being synchronized; protection is incomplete until it finishes |
| **Rebuilding** | redundancy is being restored after a disk dropped out |
| **Reshaping** | the data layout across the disks is changing |
| **Degraded (disk dropped out)** | a pool disk failed or was disconnected — see [Pool degraded](help:storage.pools.incomplete) |
| **Read-only (write error)** | the pool stopped accepting writes after an error — see [Read-only pool](help:storage.pools.readonly) |
| **Failed** | the pool does not work — usually more disks dropped out than the layout can tolerate |
| **Disks not found** | the pool disks are not present in the system |
| **State unknown** | the system could not read the pool state |

If something is wrong with a pool, the home page shows a line with an **Open** button, and the pool
card has a **What is happening** block with explanations.

## Pool types
The layout (RAID level) determines how much space is left for files and how many disks can fail
without data loss. In short:
- **Basic, JBOD, RAID 0** — no redundancy: the failure of any disk can cost the pool data.
- **RAID 1** — mirror: identical copies on all disks.
- **RAID 5 / SHR** — survives the failure of one disk; **RAID 6 / SHR-2** — of two.
- **RAID 10** — pairs of mirrors; surely survives one failure.
- **SHR and SHR-2** make better use of disks of different sizes.

Details, with a table and advice on choosing — [Pools and RAID levels](help:storage.pools.raid).

## Data integrity
- **Integrity protection** — an option in the pool creation wizard (on by default). A checksum is
  added to every data block, so silent corruption on a disk is detected on read. Writes are slower.
- **Data scrub** — periodically reads all copies of the pool data and fixes read errors. By default it
  runs every 30 days at night; the schedule is in **Settings**. See [Pool scrub](help:storage.pools.scrub).
- **Protection when redundancy is exhausted** — when one more disk failure would cause data loss, the
  pool switches to read-only by itself. See [Read-only pool](help:storage.pools.readonly).

## Pool card
Everything about one pool is here: state and RAID arrays, sync progress, **Data scrub**, **Writing to
the pool**, **Protection when redundancy is exhausted**, **Protection log** and **Delete pool**.

## What not to do
- **Delete pool** destroys all its data irreversibly. Copy off what you need first.
- Do not pull disks out of a working pool “to check”: for a pool without redundancy this means data loss.
- RAID protects against disk failure, but not against deleted files or ransomware: keep a backup of
  important data on another device and turn on shared folder snapshots.

See also: [Pools and RAID levels](help:storage.pools.raid), [Disks](help:storage.disks),
[Found pools](help:storage.pools.found), [Pool scrub](help:storage.pools.scrub).
