# Pools and RAID levels

A pool is several disks that the system combines into one storage space: from the outside you see one
place for shared folders, while inside the data is spread across the disks. How exactly it is spread
is set by the **layout** (RAID level). Two things depend on it, and they pull in opposite directions:
how much space is left for files and how many disks can fail without data loss. It is like two
identical photo albums: half the space, but losing one is no disaster.

## What you see
In the **Create pool** wizard, the **Layout** field, and on the pool card next to the state:

| Layout | Minimum disks | Space for files | Survives failure of |
|---|---|---|---|
| Basic (single disk) | exactly 1 | the whole disk | 0 disks |
| JBOD | 1 | sum of all disks | 0 disks |
| RAID 0 | 2 | sum of disks (by the smallest) | 0 disks |
| RAID 1 | 2 | one disk (the smallest) | all but one |
| RAID 5 | 3 | all disks but one | 1 disk |
| RAID 6 | 4 | all disks but two | 2 disks |
| RAID 10 | 4, an even number | half | 1 disk guaranteed |
| SHR | 1 | like RAID 5, but with disks of different sizes | 1 disk (0 with a single disk) |
| SHR-2 | 4 | like RAID 6, but with disks of different sizes | 2 disks |

Below the choice, the wizard shows the **Result**: usable capacity, how much space is unused, and
**Fault tolerance** — “survives 1 disk failure”, “won't survive a single disk failure” and so on.

## How to choose
- **Important data, 2 disks** — RAID 1.
- **3 or more identical disks** — RAID 5; if there are many disks or they are large — RAID 6: rebuilding
  redundancy on large disks takes hours, and all that time a second failure in RAID 5 means data loss.
  With more than 12 disks in RAID 5 or SHR, the wizard explicitly recommends RAID 6 / SHR-2.
- **Disks of different sizes** — SHR or SHR-2. Regular RAID takes from each disk only as much as the
  smallest one has, and the rest is wasted (the wizard shows it as unused). SHR splits the disks into
  layers by size and protects each layer separately, so less is wasted.
- **RAID 0, JBOD, Basic** — only for data you can afford to lose: the failure of any disk can cause the
  loss of pool data. The wizard warns about this.
- **RAID 10** surely survives the failure of one disk, of two — only if they are from different mirror pairs.

## Integrity protection
The **Integrity protection (dm-integrity)** switch in the wizard is on by default. A checksum is added
to every data block, and silent corruption on a disk (data reads fine but is wrong) is detected on read.
The cost: slower writes, and right after creation the pool takes longer to **sync** (the “computing
checksums” stage).

## What RAID does not do
RAID protects against disk failure, but **not against a deleted file, ransomware or fire**: a deleted
file disappears from all disks at once. Shared folder snapshots (the **Shared folders** section) and
backups on another device protect against that.

See also: [Pool degraded (disk dropped out)](help:storage.pools.incomplete),
[Read-only pool](help:storage.pools.readonly).

:::details For administrators
Capacity in the wizard is calculated before md, dm-integrity, LVM and Btrfs overhead; a system reserve
is subtracted from each disk first. SHR splits the disks into layers: a layer on 3+ disks is RAID 5, on
two — RAID 1; SHR-2: on 4+ disks — RAID 6, on three — a triple mirror. A layer without the required
protection is not used (except SHR on a single disk). The pool is built in layers: md → dm-integrity
(if enabled) → LVM → Btrfs.
:::
